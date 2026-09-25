package gateway

// 上游额度查询。只有少数供应商提供“凭 API Key 就能查”的额度接口；
// OpenAI、Anthropic 没有余额接口，配了组织级 admin key 时改查本月花费。
// 查不到的明确标注原因，不做任何猜测性展示，免得和本地估算的预算水位混为一谈。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
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
func providerUsageURL(p Provider) (addr string, ok bool, note string) {
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
	case p.Kind == "openai" || p.Kind == "anthropic":
		root := originOf(base)
		if p.AdminSecret == "" || root == "" {
			return "", false, "没有按 Key 的余额接口；填写组织 admin key 后可查本月花费"
		}
		// 两家都按天分桶，本月最多 31 桶，一页取完
		start := monthStart()
		if p.Kind == "openai" {
			return fmt.Sprintf("%s/v1/organization/costs?start_time=%d&limit=31", root, start.Unix()), true, ""
		}
		return root + "/v1/organizations/cost_report?limit=31&starting_at=" + url.QueryEscape(start.Format(time.RFC3339)), true, ""
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
	models := obj(a.Engine.Health()["models"])
	var wg sync.WaitGroup
	for i, p := range c.Providers {
		wg.Add(1)
		go func(i int, p Provider) {
			defer wg.Done()
			// 缓存里的对象是共享的，复制一份再挂限额快照
			v := maps.Clone(a.providerUsage(ctx, p))
			v["limits"] = providerLimits(c, p.ID, models)
			out[i] = v
		}(i, p)
	}
	wg.Wait()
	return out
}

// providerLimits 取该供应商各模型最近一次在响应头里声明的限额。
// 限额快照不走 60 秒缓存，额度接口查不到时供应商卡片拿它兜底展示。
func providerLimits(c Config, id string, models Object) []any {
	out := []any{}
	for _, m := range c.Models {
		h := obj(models[m.ID])
		if m.ProviderID != id || len(obj(h["limits"])) == 0 {
			continue
		}
		out = append(out, Object{"model": m.ID, "limits": h["limits"], "limits_at": h["limits_at"]})
	}
	return out
}

func (a *App) providerUsage(ctx context.Context, p Provider) Object {
	// 凭证摘要进缓存键：换 Key、清 admin key 后立即重查，不必等缓存过期
	key := p.ID + "|" + p.BaseURL + "|" + digest(p.Secret+"\x00"+p.AdminSecret)
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
	// 账单接口只认 admin key：换上它，Anthropic 走 x-api-key 与 anthropic-version
	auth, protocol := p, "chat"
	if p.Kind == "openai" || p.Kind == "anthropic" {
		auth.Secret, auth.Auth = p.AdminSecret, "auto"
		if p.Kind == "anthropic" {
			protocol = "messages"
		}
	}
	if auth.Secret == "" && auth.Auth != "none" {
		out["error"] = "未配置上游凭证"
		return out
	}
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		out["error"] = "额度接口地址无效"
		return out
	}
	requestHeaders(req, &http.Request{Header: http.Header{}}, auth, protocol, "")
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
	if p.Kind == "commandcode" {
		if len(ccWindows(o)) == 0 {
			if o2 := a.ccCreditsWithOrg(ctx, p, url); o2 != nil {
				o = o2
			}
		}
		if label, until := ccExhausted(ccWindows(o)); label != "" {
			out["exhausted_until"] = until
		}
	}
	if p.Kind == "zai" {
		if until := zaiExhausted(o); until > 0 {
			out["exhausted_until"] = until
		}
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
		infos := arr(o["balance_infos"])
		// 账号可能同时持有人民币和美元两种余额，各是一条独立记录
		if len(infos) > 1 {
			headline, fields := "", []any{}
			for _, v := range infos {
				b := obj(v)
				total := str(b, "total_balance")
				sym := currencySymbol(str(b, "currency"))
				fields = append(fields, Object{"label": str(b, "currency"), "value": sym + total})
				if headline == "" && !zeroAmount(total) {
					headline = sym + total
				}
			}
			if headline == "" && len(fields) > 0 {
				headline = str(obj(fields[0]), "value")
			}
			return headline, fields
		}
		if len(infos) == 0 {
			return "", nil
		}
		b := obj(infos[0])
		sym := currencySymbol(str(b, "currency"))
		fields := []any{}
		// 为 0 的那项不占位置，否则一眼看过去像是余额为零
		for _, x := range []struct{ key, label string }{{"granted_balance", "赠送余额"}, {"topped_up_balance", "充值余额"}} {
			if amount := str(b, x.key); amount != "" && !zeroAmount(amount) {
				fields = append(fields, Object{"label": x.label, "value": sym + amount})
			}
		}
		return sym + str(b, "total_balance"), fields
	}
	if p.Kind == "zai" {
		fields := []any{}
		headline := ""
		for _, v := range arr(obj(o["data"])["limits"]) {
			it := obj(v)
			window := zaiWindow(it)
			value := fmt.Sprintf("%.0f%%", num(it, "percentage"))
			left := ""
			if r := ccReset(it["nextResetTime"]); r > now() {
				left = durationText(r-now()) + "后"
			}
			// 主位取第一个用量窗口并自带窗口名，只写「已用 1%」看不出是哪个窗口的 1%
			if headline == "" && str(it, "type") != "TIME_LIMIT" {
				headline = window + " " + value
				if left != "" {
					// 紧跟主位放第一行，上游返回的窗口顺序不固定
					fields = append([]any{Object{"label": window + "重置", "value": left}}, fields...)
				}
				continue
			}
			if str(it, "type") == "TIME_LIMIT" {
				window = "MCP 用量 · " + window
			}
			if left != "" {
				value += " · " + left + "重置"
			}
			fields = append(fields, Object{"label": window, "value": value})
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
		// 套餐档另有 5 小时与每周两个滚动窗口，耗尽时请求会被直接拒掉，比总额更需要盯
		w := ccWindows(o)
		fields = append(fields, ccWindowFields(w)...)
		if label, _ := ccExhausted(w); label != "" {
			return label + "额度已用完", fields
		}
		return fmt.Sprintf("$%.2f", remain), fields
	}
	if p.Kind == "openai" || p.Kind == "anthropic" {
		return fmt.Sprintf("本月 $%.2f", monthCost(p.Kind, o)), nil
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

// zeroAmount 判断上游给的金额字符串是不是 0；金额是字符串，不能直接比较。
func zeroAmount(s string) bool {
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	return err == nil && v == 0
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

// monthStart 返回本月 1 日零点（UTC），两家账单接口都按 UTC 分桶。
func monthStart() time.Time {
	t := time.Now().UTC()
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// monthCost 汇总账单各天各项的金额，单位美元。
// OpenAI 的 amount 是 {value: 美元数值}；Anthropic 的 amount 是以美分计的十进制字符串。
func monthCost(kind string, o Object) float64 {
	total := 0.0
	for _, b := range arr(o["data"]) {
		for _, r := range arr(obj(b)["results"]) {
			if kind == "openai" {
				total += num(obj(obj(r)["amount"]), "value")
				continue
			}
			cents, _ := strconv.ParseFloat(str(obj(r), "amount"), 64)
			total += cents / 100
		}
	}
	return total
}

// zaiWindow 由 unit / number 写出窗口名：unit 3 = 小时、5 = 月、6 = 周。
// 老套餐的 TOKENS_LIMIT / TIME_LIMIT 可能不带 unit，按类型给默认窗口。
func zaiWindow(it Object) string {
	n := int(num(it, "number"))
	switch int(num(it, "unit")) {
	case 3:
		return fmt.Sprintf("%d 小时", n)
	case 5:
		return fmt.Sprintf("%d 个月", n)
	case 6:
		if n <= 1 {
			return "每周"
		}
		return fmt.Sprintf("%d 周", n)
	}
	if str(it, "type") == "TIME_LIMIT" {
		return "1 个月"
	}
	return "5 小时"
}

// zaiExhausted 返回已用满（percentage ≥ 100）的用量窗口中最晚的重置时刻，都没用满时为 0。
// MCP 调用次数（TIME_LIMIT）用满不影响模型推理，不计入。
func zaiExhausted(o Object) int64 {
	until := int64(0)
	for _, v := range arr(obj(o["data"])["limits"]) {
		it := obj(v)
		if str(it, "type") != "TIME_LIMIT" && num(it, "percentage") >= 100 {
			until = max(until, ccReset(it["nextResetTime"]))
		}
	}
	return until
}
