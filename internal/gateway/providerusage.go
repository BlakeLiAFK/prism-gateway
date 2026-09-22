package gateway

// 上游额度查询。只有少数供应商提供“凭 API Key 就能查”的额度接口；
// Command Code、OpenAI、Anthropic 等只能在各自控制台看，这里明确标注不支持，
// 不做任何猜测性展示，免得和本地估算的预算水位混为一谈。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

const providerUsageTTL = 60000

type usageCacheEntry struct {
	at  int64
	val Object
}

// providerUsageURL 返回额度查询地址；ok 为 false 时 note 说明原因。
func providerUsageURL(p Provider) (url string, ok bool, note string) {
	base := strings.TrimRight(p.BaseURL, "/")
	host := strings.ToLower(base)
	switch {
	case p.Kind == "mock":
		return "", false, "本地演示不消耗上游额度"
	case p.Kind == "deepseek" || strings.Contains(host, "deepseek.com"):
		// DeepSeek 的余额接口挂在根路径，不在 /v1 下
		return strings.TrimSuffix(base, "/v1") + "/user/balance", true, ""
	case strings.Contains(host, "openrouter.ai"):
		return "https://openrouter.ai/api/v1/credits", true, ""
	case p.Kind == "opencode":
		return base + "/usage", true, ""
	case p.Kind == "commandcode":
		// Provider API 那四个端点里没有额度接口，但 CLI 的 /usage 走的是同一个域名下的
		// /alpha/billing/credits，鉴权同样是 Bearer + 同一把 Key（官方文档写明
		// Provider Key 与 CLI Key 是同一把）。未公开接口，形状变了会退回通用扫描。
		if root := originOf(base); root != "" {
			return root + "/alpha/billing/credits", true, ""
		}
		return "", false, "供应商地址无法解析出额度接口"
	case p.Kind == "openai":
		return "", false, "没有按 Key 的余额接口；用量只在组织后台（需 admin key）"
	case p.Kind == "anthropic":
		return "", false, "没有按 Key 的余额接口；用量只在 Console（需 admin key）"
	case p.Kind == "zai":
		// 官方 glm-plan-usage 插件查的就是这个监控接口（未公开文档，但随插件源码发布），
		// 鉴权是裸 token、不加 Bearer 前缀。地址跟随 base_url 的域名，
		// 智谱开放平台（open.bigmodel.cn）用同一套路径。
		if root := originOf(base); root != "" {
			return root + "/api/monitor/usage/quota/limit", true, ""
		}
		return "", false, "供应商地址无法解析出额度接口"
	}
	return "", false, "该供应商未提供可查询的额度接口"
}

// originOf 取地址的 scheme://host，用于拼同域名下的其它接口。
func originOf(base string) string {
	u, err := url.Parse(base)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

func (a *App) providerUsageAll(ctx context.Context) []any {
	c := a.Store.Config()
	out := make([]any, len(c.Providers))
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	for i, p := range c.Providers {
		wg.Add(1)
		go func(i int, p Provider) {
			defer wg.Done()
			out[i] = a.providerUsage(ctx, p)
		}(i, p)
	}
	wg.Wait()
	return out
}

func (a *App) providerUsage(ctx context.Context, p Provider) Object {
	key := p.ID + "|" + p.BaseURL
	a.usageMu.Lock()
	if e, okc := a.usageCache[key]; okc && now()-e.at < providerUsageTTL {
		a.usageMu.Unlock()
		return e.val
	}
	a.usageMu.Unlock()
	v := a.fetchProviderUsage(ctx, p)
	a.usageMu.Lock()
	if a.usageCache == nil {
		a.usageCache = map[string]usageCacheEntry{}
	}
	a.usageCache[key] = usageCacheEntry{now(), v}
	a.usageMu.Unlock()
	return v
}

func (a *App) fetchProviderUsage(ctx context.Context, p Provider) Object {
	url, ok, note := providerUsageURL(p)
	out := Object{"id": p.ID, "supported": ok, "note": note, "fetched_at": now()}
	if !ok {
		return out
	}
	if !p.HasKey && p.Auth != "none" {
		out["error"] = "未配置上游凭证"
		return out
	}
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		out["error"] = "额度接口地址无效"
		return out
	}
	requestHeaders(req, &http.Request{Header: http.Header{}}, p, "chat", "")
	if p.Kind == "zai" && p.Secret != "" {
		// 监控接口要的是裸 token，带 Bearer 前缀会被判为未鉴权
		req.Header.Set("Authorization", p.Secret)
		req.Header.Set("Accept-Language", "en-US,en")
	}
	res, err := a.Engine.client(p).Do(req)
	if err != nil {
		out["error"] = "上游连接失败"
		return out
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 256<<10))
	out["status"] = res.StatusCode
	if res.StatusCode != 200 {
		out["error"] = fmt.Sprintf("上游返回 HTTP %d", res.StatusCode)
		return out
	}
	var o Object
	if json.Unmarshal(body, &o) != nil {
		out["error"] = "额度接口返回了无法解析的内容"
		return out
	}
	// Z.AI 这类接口鉴权失败也回 HTTP 200，错误藏在 body 的业务码里
	if o["success"] == false {
		msg := str(o, "msg")
		if msg == "" {
			msg = "额度接口返回失败"
		}
		out["error"] = msg
		return out
	}
	headline, fields := parseUsage(p, o)
	out["headline"] = headline
	out["fields"] = fields
	return out
}

// parseUsage 把各家形状不同的额度响应压成统一展示结构：
// 一个主数值 headline，加若干明细字段。已知形状精确解析，
// 未知形状回退到通用扫描——上游改字段时至少还能显示原始数值，而不是空卡片。
func parseUsage(p Provider, o Object) (string, []any) {
	host := strings.ToLower(p.BaseURL)
	if p.Kind == "deepseek" || strings.Contains(host, "deepseek.com") {
		for _, v := range arr(o["balance_infos"]) {
			b := obj(v)
			sym := currencySymbol(str(b, "currency"))
			return sym + str(b, "total_balance"), []any{
				Object{"label": "赠送余额", "value": sym + str(b, "granted_balance")},
				Object{"label": "充值余额", "value": sym + str(b, "topped_up_balance")},
			}
		}
		return "", nil
	}
	if p.Kind == "zai" {
		labels := map[string]string{"TOKENS_LIMIT": "Token 用量 · 5 小时", "TIME_LIMIT": "MCP 用量 · 1 个月"}
		fields := []any{}
		headline := ""
		for _, v := range arr(obj(o["data"])["limits"]) {
			it := obj(v)
			kind := str(it, "type")
			label := labels[kind]
			if label == "" {
				label = kind
			}
			value := fmt.Sprintf("%.0f%%", num(it, "percentage"))
			if kind == "TOKENS_LIMIT" {
				// 主位要自带窗口名，只写「已用 1%」看不出是哪个窗口的 1%
				headline = "5 小时 " + value
				continue
			}
			fields = append(fields, Object{"label": label, "value": value})
		}
		return headline, fields
	}
	if p.Kind == "commandcode" {
		c := obj(o["credits"])
		remain := num(c, "monthlyCredits") + num(c, "purchasedCredits") + num(c, "freeCredits")
		fields := []any{
			Object{"label": "月度剩余", "value": fmt.Sprintf("$%.2f", num(c, "monthlyCredits"))},
			Object{"label": "附加额度", "value": fmt.Sprintf("$%.2f", num(c, "purchasedCredits")+num(c, "freeCredits"))},
		}
		// 套餐档另有 5 小时与 7 天两个滚动窗口，耗尽时请求会被直接拒掉，比总额更需要盯
		w := obj(c["windowLimits"])
		for _, x := range []struct{ key, label string }{{"fiveHour", "5 小时"}, {"weekly", "本周"}} {
			if v := obj(w[x.key]); v != nil {
				fields = append(fields, Object{"label": x.label, "value": fmt.Sprintf("$%.2f / $%.0f", num(v, "used"), num(v, "cap"))})
			}
		}
		return fmt.Sprintf("$%.2f", remain), fields
	}
	if strings.Contains(host, "openrouter.ai") {
		d := obj(o["data"])
		total, used := num(d, "total_credits"), num(d, "total_usage")
		return fmt.Sprintf("$%.2f", total-used), []any{
			Object{"label": "已充值", "value": fmt.Sprintf("$%.2f", total)},
			Object{"label": "已消耗", "value": fmt.Sprintf("$%.2f", used)},
		}
	}
	fields := scanUsageFields(o)
	// 未知形状：把最像余额的字段提到主位，剩下的作明细，卡片上不至于只显示一个破折号
	for i, v := range fields {
		label := str(obj(v), "label")
		if strings.Contains(label, "balance") || strings.Contains(label, "credit") || strings.Contains(label, "remaining") {
			return str(obj(v), "value"), append(fields[:i:i], fields[i+1:]...)
		}
	}
	return "", fields
}

// scanUsageFields 通用扫描：取顶层与 data 层的标量值，最多 8 条。
// 不猜语义，字段名原样显示，让人自己看懂上游回了什么。
func scanUsageFields(o Object) []any {
	fields := []any{}
	collect := func(prefix string, src Object) {
		for _, k := range sortedKeys(src) {
			if len(fields) >= 8 {
				return
			}
			s, ok := scalarText(src[k])
			if !ok {
				continue
			}
			fields = append(fields, Object{"label": prefix + k, "value": s})
		}
	}
	collect("", o)
	if d := obj(o["data"]); d != nil {
		collect("data.", d)
	}
	return fields
}

func scalarText(v any) (string, bool) {
	switch x := v.(type) {
	case string:
		if x == "" || len(x) > 40 {
			return "", false
		}
		return x, true
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64), true
	case bool:
		return map[bool]string{true: "是", false: "否"}[x], true
	}
	return "", false
}

func currencySymbol(c string) string {
	switch strings.ToUpper(c) {
	case "CNY", "RMB":
		return "¥"
	case "USD", "":
		return "$"
	}
	return c + " "
}

func sortedKeys(o Object) []string {
	ks := make([]string, 0, len(o))
	for k := range o {
		ks = append(ks, k)
	}
	slices.Sort(ks)
	return ks
}
