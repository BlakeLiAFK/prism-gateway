package gateway

// 告警推送：上游额度用满、模型连续失败、路由无可用候选时，主动推到 Telegram 或 Webhook。
// 目标地址与 Token 是凭证，加密存在 meta 表，接口只回「是否已设置」，不进配置快照与导出。
// 同一事件 30 分钟内只推一次，推送在后台进行，失败只记日志，不影响请求。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

// alertConfig 存在 meta.alert_config；Secret 是 Telegram Bot Token 或 Webhook URL，加密保存
type alertConfig struct {
	Enabled bool            `json:"enabled"`
	Kind    string          `json:"kind"` // telegram | webhook（JSON）| webhook_text（纯文本）
	ChatID  string          `json:"chat_id"`
	Secret  string          `json:"secret"`
	Events  map[string]bool `json:"events"` // quota | failures | route
}

const alertCooldown = 30 * 60000

var alertEvents = []string{"quota", "failures", "route"}

type alerter struct {
	mu   sync.Mutex
	sent map[string]int64
}

func init() {
	consoleActions["alert.get"] = func(a *App, _ string, _ Object) (any, error) { return a.alertView() }
	consoleActions["alert.save"] = func(a *App, _ string, p Object) (any, error) { return a.saveAlert(p) }
	consoleActions["alert.test"] = func(a *App, _ string, _ Object) (any, error) {
		c, err := a.loadAlert()
		if err != nil {
			return nil, err
		}
		if c.Secret == "" {
			return nil, fail("INVALID_PARAMS", "请先填写并保存推送目标", 400)
		}
		if err = a.deliver(a.Context, c, "Prism Gateway 测试消息：告警推送已连通。"); err != nil {
			return nil, fail("ALERT_FAILED", "推送失败："+err.Error(), 502)
		}
		return Object{"sent": true}, nil
	}
}

func (a *App) loadAlert() (alertConfig, error) {
	c := alertConfig{Kind: "telegram", Events: map[string]bool{}}
	rows, err := a.Store.DB.Query("SELECT value FROM meta WHERE key='alert_config'")
	if err != nil || len(rows) == 0 {
		return c, err
	}
	if err = json.Unmarshal([]byte(rows[0].String("value")), &c); err != nil {
		return c, err
	}
	if c.Secret != "" {
		if c.Secret, err = a.Store.decrypt(c.Secret); err != nil {
			return c, err
		}
	}
	return c, nil
}

// alertView 返回给界面的配置：不含 Token / URL，只说明是否已设置
func (a *App) alertView() (any, error) {
	c, err := a.loadAlert()
	if err != nil {
		return nil, err
	}
	return Object{"enabled": c.Enabled, "kind": c.Kind, "chat_id": c.ChatID, "has_secret": c.Secret != "", "events": c.Events}, nil
}

func (a *App) saveAlert(p Object) (any, error) {
	c, err := a.loadAlert()
	if err != nil {
		return nil, err
	}
	c.Enabled, c.Kind, c.ChatID = boolean(p, "enabled"), str(p, "kind"), strings.TrimSpace(str(p, "chat_id"))
	if c.Kind != "telegram" && c.Kind != "webhook" && c.Kind != "webhook_text" {
		return nil, fail("INVALID_PARAMS", "推送方式只支持 telegram / webhook / webhook_text", 400)
	}
	// secret 留空表示沿用已保存的值，界面不必回显凭证
	if s := strings.TrimSpace(str(p, "secret")); s != "" {
		if c.Kind != "telegram" && !strings.HasPrefix(s, "https://") && !strings.HasPrefix(s, "http://") {
			return nil, fail("INVALID_PARAMS", "Webhook 地址需以 http:// 或 https:// 开头", 400)
		}
		c.Secret = s
	}
	if c.Enabled && (c.Secret == "" || (c.Kind == "telegram" && c.ChatID == "")) {
		return nil, fail("INVALID_PARAMS", "启用前需填写 Telegram Bot Token 与 Chat ID，或 Webhook 地址", 400)
	}
	c.Events = map[string]bool{}
	for _, k := range alertEvents {
		c.Events[k] = boolean(obj(p["events"]), k)
	}
	stored := c
	stored.Secret = a.Store.encrypt(c.Secret)
	if err = a.Store.DB.Exec("INSERT INTO meta VALUES ('alert_config', ?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", raw(stored)); err != nil {
		return nil, err
	}
	a.audit("alert.save", c.Kind)
	return a.alertView()
}

// alert 推送一条事件；key 用于限频（同一 key 30 分钟一次），event 对应可勾选的事件类别
func (a *App) alert(event, key, text string) {
	c, err := a.loadAlert()
	if err != nil || !c.Enabled || c.Secret == "" || !c.Events[event] {
		return
	}
	a.alerts.mu.Lock()
	if a.alerts.sent == nil {
		a.alerts.sent = map[string]int64{}
	}
	if now()-a.alerts.sent[key] < alertCooldown {
		a.alerts.mu.Unlock()
		return
	}
	a.alerts.sent[key] = now()
	a.alerts.mu.Unlock()
	a.workers.Add(1)
	go func() {
		defer a.workers.Done()
		if err := a.deliver(a.Context, c, "【Prism Gateway】\n"+text); err != nil {
			slog.Error("alert delivery failed", "kind", c.Kind, "event", event, "err", err)
		}
	}()
}

// notify 直接推送，不经事件勾选与 30 分钟限频：定时任务有自己的开关与去重。
// 推送通道未启用或未配置时返回错误，由调用方记为本次运行失败。
func (a *App) notify(text string) error {
	c, err := a.loadAlert()
	if err != nil {
		return err
	}
	if !c.Enabled || c.Secret == "" {
		return fail("ALERT_DISABLED", "推送通道未启用：请先在「告警推送」里配置并启用", 400)
	}
	return a.deliver(a.Context, c, "【Prism Gateway】\n"+text)
}

func (a *App) deliver(ctx context.Context, c alertConfig, text string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	addr, body, ctype := c.Secret, raw(Object{"event": "prism.alert", "text": text, "at": now()}), "application/json"
	switch c.Kind {
	case "telegram":
		addr, body = "https://api.telegram.org/bot"+c.Secret+"/sendMessage", raw(Object{"chat_id": c.ChatID, "text": text})
	case "webhook_text":
		// 原样 POST 消息正文，换行保留，适合直接转发到只收文本的机器人或日志端点
		body, ctype = text, "text/plain; charset=utf-8"
	}
	req, err := http.NewRequestWithContext(ctx, "POST", addr, bytes.NewBufferString(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", ctype)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		// 错误信息里可能带着含 Token 的地址，只回报类别
		return fmt.Errorf("无法连接推送目标")
	}
	defer res.Body.Close()
	if res.StatusCode >= 300 {
		return fmt.Errorf("推送目标返回 HTTP %d", res.StatusCode)
	}
	return nil
}
