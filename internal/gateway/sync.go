// 从上游同步模型清单的后台任务。
package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

func providerProtocol(p Provider) string {
	// Z.AI 同时提供 OpenAI 兼容面和 Anthropic 兼容面，由填入的地址决定协议
	if p.Kind == "anthropic" || (p.Kind == "zai" && strings.Contains(p.BaseURL, "/anthropic")) {
		return "messages"
	}
	return "chat"
}

func (a *App) startSync(id string) (any, error) {
	c := a.Store.Config()
	p, ok := c.provider(id)
	if !ok {
		return nil, fail("NOT_FOUND", "Provider 不存在", 404)
	}
	if p.Kind == "mock" {
		return nil, fail("NOT_SUPPORTED", "本地演示不需要同步模型", 400)
	}
	jid := randomID("job_")
	er := a.Store.DB.Exec("INSERT INTO jobs(id,action,status,created_at,updated_at) VALUES (?,?,'queued',?,?)", jid, "provider.sync_models", now(), now())
	if er != nil {
		return nil, er
	}
	a.audit("provider.sync_models", id)
	a.workers.Add(1)
	go func() {
		defer a.workers.Done()
		a.Store.DB.Exec("UPDATE jobs SET status='running',updated_at=? WHERE id=?", now(), jid)
		ctx, cancel := context.WithTimeout(a.Context, 30*time.Second)
		defer cancel()
		result, err := a.syncModels(ctx, p, c.Version)
		if err != nil {
			msg := "同步失败：上游连接、响应格式或配置版本发生变化，请检查后重试。"
			var ae *APIError
			if errors.As(err, &ae) {
				msg = ae.Message
			}
			a.Store.DB.Exec("UPDATE jobs SET status='failed',error=?,updated_at=? WHERE id=?", msg, now(), jid)
		} else {
			a.Store.DB.Exec("UPDATE jobs SET status='succeeded',result=?,updated_at=? WHERE id=?", raw(result), now(), jid)
		}
	}()
	return Object{"job_id": jid}, nil
}

func (a *App) syncModels(ctx context.Context, p Provider, version int64) (any, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", strings.TrimRight(p.BaseURL, "/")+"/models", nil)
	requestHeaders(req, &http.Request{Header: http.Header{}}, p, providerProtocol(p), "")
	res, err := a.Engine.client(p).Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return nil, fail("UPSTREAM_ERROR", fmt.Sprintf("模型列表返回 HTTP %d", res.StatusCode), 502)
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	var o Object
	if err = json.Unmarshal(data, &o); err != nil {
		return nil, err
	}
	items := arr(o["data"])
	if len(items) == 0 || len(items) > 2000 {
		return nil, fail("INVALID_MODELS", "未识别到模型列表，或模型数量超过 2000；可在界面手动添加", 400)
	}
	added := 0
	_, err = a.Store.Change(version, "provider.sync_models", p.ID, func(c *Config) error {
		for _, v := range items {
			v := obj(v)
			up := str(v, "id")
			if !validID(up) {
				continue
			}
			exists := false
			for _, m := range c.Models {
				if m.ProviderID == p.ID && m.Upstream == up {
					exists = true
					break
				}
			}
			if exists {
				continue
			}
			protocol := providerProtocol(p)
			switch p.Kind {
			case "opencode":
				protocol = openCodeProtocol(up)
			case "commandcode":
				protocol = commandCodeProtocol(v)
			}
			name := str(v, "name")
			if name == "" {
				name = str(v, "display_name")
			}
			if name == "" {
				name = up
			}
			// 模型 ID 直接用上游原名，客户端传 model 时更直观；
			// 与已有对象重名时才退回带 provider 前缀的形式
			mid := up
			if !validID(mid) || c.nameTaken(mid) {
				mid = p.ID + "/" + up
			}
			if !validID(mid) {
				mid = "model_" + digest(p.ID + up)[:20]
			}
			c.Models = append(c.Models, newSyncedModel(mid, p.ID, up, name, protocol, v))
			added++
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return Object{"added": added, "note": "新模型默认禁用。请确认原生协议、能力、上下文和价格，再手动启用；未自动抓取官方额度。"}, nil
}

// commandCodeProtocol 按上游 supported_endpoints 判定原生协议：
// Command Code 在模型列表里直接声明了每个模型能走哪些端点，不需要像
// OpenCode 那样维护模型名表。同时支持多个端点时取兼容性最好的一个。
func commandCodeProtocol(src Object) string {
	eps := map[string]bool{}
	for _, v := range arr(src["supported_endpoints"]) {
		s, _ := v.(string)
		eps[s] = true
	}
	switch {
	case eps["/messages"]:
		return "messages"
	case eps["/chat/completions"]:
		return "chat"
	case eps["/responses"]:
		return "responses"
	}
	return "chat"
}

func openCodeProtocol(id string) string {
	switch id {
	case "minimax-m3", "minimax-m2.7", "minimax-m2.5", "qwen3.8-max", "qwen3.8-flash", "qwen3.7-max", "qwen3.7-plus", "qwen3.6-plus":
		return "messages"
	case "gpt-5.6-luna", "grok-4.6", "muse-spark-1.3-contributor", "muse-spark-1.2-contributor":
		return "responses"
	default:
		return "chat"
	}
}
