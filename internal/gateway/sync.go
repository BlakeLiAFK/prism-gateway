// 从上游同步模型清单的后台任务。
package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
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
		a.jobWrite("UPDATE jobs SET status='running',updated_at=? WHERE id=?", now(), jid)
		ctx, cancel := context.WithTimeout(a.Context, 30*time.Second)
		defer cancel()
		result, err := a.syncModels(ctx, p, c.Version)
		if err != nil {
			msg := "同步失败：上游连接、响应格式或配置版本发生变化，请检查后重试。"
			var ae *APIError
			if errors.As(err, &ae) {
				msg = ae.Message
			}
			a.jobWrite("UPDATE jobs SET status='failed',error=?,updated_at=? WHERE id=?", msg, now(), jid)
		} else {
			a.jobWrite("UPDATE jobs SET status='succeeded',result=?,updated_at=? WHERE id=?", raw(result), now(), jid)
		}
	}()
	return Object{"job_id": jid}, nil
}

func (a *App) syncModels(ctx context.Context, p Provider, version int64) (any, error) {
	items, err := a.upstreamModels(ctx, p)
	if err != nil {
		return nil, err
	}
	return a.applySyncedModels(p, version, items)
}

// upstreamModels 取上游 /models 列表的 data 数组
func (a *App) upstreamModels(ctx context.Context, p Provider) ([]any, error) {
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
	return items, nil
}

func (a *App) applySyncedModels(p Provider, version int64, items []any) (any, error) {
	added, updated := 0, 0
	_, err := a.Store.Change(version, "provider.sync_models", p.ID, func(c *Config) error {
		// 官方 DeepSeek 的 Messages 使用独立前缀；自定义地址必须由管理员指定。
		if p.Kind == "deepseek" && (p.BaseURL == "https://api.deepseek.com" || p.BaseURL == "https://api.deepseek.com/v1") && p.ProtocolURLs["messages"] == "" {
			for i := range c.Providers {
				if c.Providers[i].ID == p.ID {
					if c.Providers[i].ProtocolURLs == nil {
						c.Providers[i].ProtocolURLs = map[string]string{}
					}
					c.Providers[i].ProtocolURLs["messages"] = "https://api.deepseek.com/anthropic/v1"
				}
			}
		}
		for _, v := range items {
			v := obj(v)
			up := str(v, "id")
			if !validID(up) {
				continue
			}
			exists := false
			for i, m := range c.Models {
				if m.ProviderID == p.ID && m.Upstream == up {
					protocols := syncedProtocols(p, v)
					if !m.ProtocolsLocked && protocols != nil && (m.Protocols == nil || !slices.Equal(protocols, m.Protocols)) {
						c.Models[i].Protocols = protocols
						if len(protocols) > 0 && !slices.Contains(protocols, m.Protocol) {
							c.Models[i].Protocol = protocols[0]
						}
						updated++
					}
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
			m := newSyncedModel(mid, p.ID, up, name, protocol, v)
			m.Protocols = syncedProtocols(p, v)
			if p.Kind == "commandcode" && m.Protocols == nil {
				m.Protocols = []string{}
			}
			if len(m.Protocols) > 0 && !slices.Contains(m.Protocols, m.Protocol) {
				m.Protocol = m.Protocols[0]
			}
			c.Models = append(c.Models, m)
			added++
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return Object{"added": added, "updated": updated, "note": "已刷新未锁定的协议声明；工具与图像未声明时标为未知，由上游判定。既有能力开关、名称、价格与限额保持不变。"}, nil
}

func syncedProtocols(p Provider, src Object) []string {
	if isOpenRouter(p) {
		return []string{"chat", "messages", "responses"}
	}
	// DeepSeek /models 没有端点字段，以下来自官方公开的当前模型能力表。
	if p.Kind == "deepseek" && (str(src, "id") == "deepseek-flash" || str(src, "id") == "deepseek-v4-pro") {
		return []string{"chat", "messages", "responses"}
	}
	value, declared := src["supported_endpoints"].([]any)
	if !declared {
		return nil
	}
	out := []string{}
	for _, protocol := range []string{"messages", "chat", "responses", "systemone"} {
		for _, endpoint := range arr(value) {
			if endpoint == pathFor(protocol) {
				out = append(out, protocol)
				break
			}
		}
	}
	return out
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

// jobWrite 写任务状态；后台任务没有调用方可以接收错误，失败必须落日志
func (a *App) jobWrite(query string, args ...any) {
	if err := a.Store.DB.Exec(query, args...); err != nil {
		slog.Error("job status write failed", "err", err)
	}
}
