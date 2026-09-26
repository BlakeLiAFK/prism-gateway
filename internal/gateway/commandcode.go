package gateway

// Command Code 的套餐窗口额度（5 小时 / 每周）。
// CLI 的 /usage 从 /alpha/billing/credits 响应的顶层读 windowLimits；
// 窗口耗尽时上游只回 429、不带任何限额响应头，只能靠这里得知何时恢复。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

var ccWindowLabels = []struct{ key, label string }{{"fiveHour", "5 小时"}, {"weekly", "本周"}}

// ccWindows 取窗口额度；早先误以为在 credits 内层，两处都认
func ccWindows(o Object) Object {
	if w := obj(o["windowLimits"]); len(w) > 0 {
		return w
	}
	return obj(obj(o["credits"])["windowLimits"])
}

// ccReset 把 resetAt 解析成毫秒时间戳，解析不了返回 0
func ccReset(v any) int64 {
	switch x := v.(type) {
	case float64:
		return parseReset(strconv.FormatFloat(x, 'f', -1, 64))
	case string:
		return parseReset(x)
	}
	return 0
}

// ccExhausted 返回已耗尽的窗口名与其中最晚的重置时刻；都没耗尽时窗口名为空
func ccExhausted(w Object) (label string, until int64) {
	for _, x := range ccWindowLabels {
		v := obj(w[x.key])
		if v == nil || num(v, "cap") <= 0 || num(v, "used") < num(v, "cap") {
			continue
		}
		if r := ccReset(v["resetAt"]); label == "" || r > until {
			label, until = x.label, r
		}
	}
	return label, until
}

// quotaWindows 把 Command Code / Z.AI 的窗口额度统一成 [{label, percent, reset_at}]，供额度预警使用。
// Z.AI 的 MCP 调用次数（TIME_LIMIT）不影响模型推理，不计入。
func quotaWindows(kind string, o Object) []any {
	out := []any{}
	add := func(label string, pct float64, reset int64) {
		out = append(out, Object{"label": label, "percent": pct, "reset_at": reset})
	}
	switch kind {
	case "commandcode":
		w := ccWindows(o)
		for _, x := range ccWindowLabels {
			if v := obj(w[x.key]); v != nil && num(v, "cap") > 0 {
				add(x.label, num(v, "used")/num(v, "cap")*100, ccReset(v["resetAt"]))
			}
		}
	case "zai":
		for _, v := range arr(obj(o["data"])["limits"]) {
			if it := obj(v); str(it, "type") != "TIME_LIMIT" {
				add(zaiWindow(it), num(it, "percentage"), ccReset(it["nextResetTime"]))
			}
		}
	}
	return out
}

// ccWindowFields 生成「已用 / 上限 · 多久后重置」明细
func ccWindowFields(w Object) []any {
	out := []any{}
	for _, x := range ccWindowLabels {
		v := obj(w[x.key])
		if v == nil {
			continue
		}
		value := fmt.Sprintf("$%.2f / $%.0f", num(v, "used"), num(v, "cap"))
		if r := ccReset(v["resetAt"]); r > now() {
			value += " · " + durationText(r-now()) + "后重置"
		}
		out = append(out, Object{"label": x.label, "value": value})
	}
	return out
}

// durationText 把毫秒时长写成「3 天 4 小时」「5 小时 12 分」「12 分钟」
func durationText(ms int64) string {
	d := time.Duration(ms) * time.Millisecond
	day, hour, minute := int(d.Hours())/24, int(d.Hours())%24, int(d.Minutes())%60
	switch {
	case day > 0:
		return fmt.Sprintf("%d 天 %d 小时", day, hour)
	case hour > 0:
		return fmt.Sprintf("%d 小时 %d 分", hour, minute)
	}
	return fmt.Sprintf("%d 分钟", max(minute, 1))
}

// ccCreditsWithOrg 在不带 orgId 查不到窗口时，按 CLI 的做法先经 /alpha/whoami
// 取组织 ID，再带 ?orgId= 重查一次。失败返回 nil，调用方沿用第一次的结果。
func (a *App) ccCreditsWithOrg(ctx context.Context, p Provider, creditsURL string) Object {
	root := originOf(p.BaseURL)
	who := a.ccGet(ctx, p, root+"/alpha/whoami?limits=1")
	org := str(obj(who["org"]), "id")
	if org == "" {
		return nil
	}
	return a.ccGet(ctx, p, creditsURL+"?orgId="+url.QueryEscape(org))
}

func (a *App) ccGet(ctx context.Context, p Provider, addr string) Object {
	req, err := http.NewRequestWithContext(ctx, "GET", addr, nil)
	if err != nil {
		return nil
	}
	requestHeaders(req, &http.Request{Header: http.Header{}}, p, "chat", "")
	res, err := a.Engine.client(p).Do(req)
	if err != nil {
		return nil
	}
	defer res.Body.Close()
	var o Object
	if res.StatusCode != 200 || json.NewDecoder(io.LimitReader(res.Body, 256<<10)).Decode(&o) != nil {
		return nil
	}
	return o
}

// checkWindows 在 Command Code 或 Z.AI 返回 429 后查窗口额度：已耗尽就把该供应商的
// 全部模型冷却到重置时刻，新请求不必每隔 30 秒再撞一次 429 才切换。
func (a *App) checkWindows(p Provider) {
	if p.Kind != "commandcode" && p.Kind != "zai" {
		return
	}
	until, _ := a.providerUsage(a.Context, p)["exhausted_until"].(int64)
	if until <= now() {
		return
	}
	for _, m := range a.Store.Config().Models {
		if m.ProviderID == p.ID {
			a.Engine.coolUntil(m.ID, until)
		}
	}
	a.alert("quota", "quota:"+p.ID, fmt.Sprintf("%s 的额度窗口已用完，旗下模型冷却中，约 %s后恢复。", p.Name, durationText(until-now())))
}

// coolUntil 冷却到指定时刻。与响应头声明的耗尽冷却一样封顶 24 小时：
// 周额度可能要几天后才恢复，到期仍耗尽时下一次 429 会再查一次。
func (e *Engine) coolUntil(id string, until int64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	st := e.state(id)
	if until = min(until, now()+int64(24*time.Hour/time.Millisecond)); until > st.Cooldown {
		st.Cooldown = until
	}
}
