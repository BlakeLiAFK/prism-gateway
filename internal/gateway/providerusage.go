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
		return "", false, "Provider API 未提供额度接口；额度见 CLI /usage 或 Studio"
	case p.Kind == "openai":
		return "", false, "没有按 Key 的余额接口；用量只在组织后台（需 admin key）"
	case p.Kind == "anthropic":
		return "", false, "没有按 Key 的余额接口；用量只在 Console（需 admin key）"
	case p.Kind == "zai":
		return "", false, "未公开按 Key 的额度接口；额度见官方控制台"
	}
	return "", false, "该供应商未提供可查询的额度接口"
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
