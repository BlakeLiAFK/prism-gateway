package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
)

const Version = "1.12.0"

type Object = map[string]any

type Provider struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Kind         string `json:"kind"`
	BaseURL      string `json:"base_url"`
	Auth         string `json:"auth"`
	Enabled      bool   `json:"enabled"`
	AllowPrivate bool   `json:"allow_private"`
	TimeoutSec   int    `json:"timeout_sec"`
	HasKey       bool   `json:"has_key"`
	Secret       string `json:"-"`
	Description  string `json:"description"`
}
type Model struct {
	ID          string `json:"id"`
	ProviderID  string `json:"provider_id"`
	Upstream    string `json:"upstream"`
	Name        string `json:"name"`
	Protocol    string `json:"protocol"`
	Enabled     bool   `json:"enabled"`
	Tools       bool   `json:"tools"`
	Vision      bool   `json:"vision"`
	NativeCount bool   `json:"native_count"`
	// 上游会返回推理内容而又需要跨协议调用时，由管理员显式打开。
	// 打开后跨协议转换会丢弃推理内容，并在响应头 X-Prism-Dropped 中标注。
	DropReasoning bool    `json:"drop_reasoning"`
	Context       int     `json:"context_window"`
	MaxOutput     int     `json:"max_output_tokens"`
	Concurrency   int     `json:"concurrency"`
	RPM           int     `json:"rpm"`
	InputPrice    float64 `json:"input_price"`
	OutputPrice   float64 `json:"output_price"`
	CachePrice    float64 `json:"cache_price"`
	WritePrice    float64 `json:"write_price"`
	PricingSet    bool    `json:"pricing_set"`
	Limit5h       float64 `json:"limit_5h"`
	Limit7d       float64 `json:"limit_7d"`
	Limit30d      float64 `json:"limit_30d"`
}
type Candidate struct {
	ModelID string `json:"model_id"`
	Weight  int    `json:"weight"`
}
type Route struct {
	ID          string      `json:"id"`
	Name        string      `json:"name"`
	Strategy    string      `json:"strategy"`
	Enabled     bool        `json:"enabled"`
	Affinity    bool        `json:"affinity"`
	Candidates  []Candidate `json:"candidates"`
	Description string      `json:"description"`
	// Sort 决定路由在管理界面里的先后。按 ID 排出来的是字母序，
	// 与 lite / auto / pro / max 这种能力梯度无关，只能显式存。
	Sort int `json:"sort"`
}
type Alias struct {
	ID      string `json:"id"`
	Target  string `json:"target"`
	Enabled bool   `json:"enabled"`
}
type Settings struct {
	AppName             string `json:"app_name"`
	DefaultRoute        string `json:"default_route"`
	RetentionDays       int    `json:"retention_days"`
	MaxBodyMB           int    `json:"max_body_mb"`
	GlobalConcurrency   int    `json:"global_concurrency"`
	SessionTTLHours     int    `json:"session_ttl_hours"`
	AllowEstimatedCount bool   `json:"allow_estimated_count"`
	Listen              string `json:"listen"`
	MetricsEnabled      bool   `json:"metrics_enabled"`
	LogLevel            string `json:"log_level"`
	LogFormat           string `json:"log_format"`
}
type Config struct {
	Version   int64      `json:"version"`
	Providers []Provider `json:"providers"`
	Models    []Model    `json:"models"`
	Routes    []Route    `json:"routes"`
	Aliases   []Alias    `json:"aliases"`
	Settings  Settings   `json:"settings"`
}

func defaults() Config {
	return Config{Version: 1, Providers: []Provider{}, Models: []Model{}, Routes: []Route{}, Aliases: []Alias{}, Settings: Settings{AppName: "Prism Gateway", DefaultRoute: "auto-coding", RetentionDays: 90, MaxBodyMB: 8, GlobalConcurrency: 8, SessionTTLHours: 24, AllowEstimatedCount: true, Listen: "127.0.0.1:8080", LogLevel: "info", LogFormat: "text"}}
}
func (c Config) provider(id string) (Provider, bool) {
	for _, p := range c.Providers {
		if p.ID == id {
			return p, true
		}
	}
	return Provider{}, false
}
func (c Config) model(id string) (Model, bool) {
	for _, m := range c.Models {
		if m.ID == id {
			return m, true
		}
	}
	return Model{}, false
}
func (c Config) route(id string) (Route, bool) {
	for _, r := range c.Routes {
		if r.ID == id {
			return r, true
		}
	}
	return Route{}, false
}
func str(o Object, k string) string { s, _ := o[k].(string); return s }
func num(o Object, k string) float64 {
	switch n := o[k].(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case int64:
		return float64(n)
	case json.Number:
		f, _ := n.Float64()
		return f
	}
	return 0
}
func boolean(o Object, k string) bool { b, _ := o[k].(bool); return b }
func obj(v any) Object                { m, _ := v.(map[string]any); return m }
func arr(v any) []any                 { a, _ := v.([]any); return a }
func raw(v any) string                { b, _ := json.Marshal(v); return string(b) }
func now() int64                      { return time.Now().UnixMilli() }
func nano(v float64) int64            { return int64(math.Ceil(v * 1e9)) }
func dollars(n int64) float64         { return float64(n) / 1e9 }
func clamp(n, lo, hi int) int {
	if n < lo {
		return lo
	}
	if n > hi {
		return hi
	}
	return n
}
func validID(s string) bool {
	if len(s) < 1 || len(s) > 160 {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("-_.:/", c)) {
			return false
		}
	}
	return !strings.Contains(s, "..")
}
func decode(v any, dst any) error {
	b, e := json.Marshal(v)
	if e != nil {
		return e
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	return dec.Decode(dst)
}
func (c Config) Validate() error {
	names := map[string]bool{}
	for _, p := range c.Providers {
		if !validID(p.ID) || strings.TrimSpace(p.Name) == "" {
			return errors.New("Provider ID / 名称不能为空")
		}
		if names["p:"+p.ID] {
			return errors.New("Provider ID 重复")
		}
		names["p:"+p.ID] = true
		if p.Kind != "mock" {
			if e := validateBaseURL(p.BaseURL, p.AllowPrivate); e != nil {
				return e
			}
		}
		if p.Auth != "auto" && p.Auth != "bearer" && p.Auth != "x-api-key" && p.Auth != "none" {
			return errors.New("auth 必须是 auto / bearer / x-api-key / none")
		}
		if p.TimeoutSec < 5 || p.TimeoutSec > 1800 {
			return errors.New("timeout_sec 必须介于 5 和 1800")
		}
	}
	for _, m := range c.Models {
		if !validID(m.ID) || !validID(m.Upstream) {
			return errors.New("模型 ID 不合法")
		}
		if names[m.ID] {
			return errors.New("模型/路由/别名 ID 重复")
		}
		names[m.ID] = true
		if _, ok := c.provider(m.ProviderID); !ok {
			return fmt.Errorf("模型 %s 的 Provider 不存在", m.ID)
		}
		if m.Protocol != "chat" && m.Protocol != "messages" && m.Protocol != "responses" && m.Protocol != "systemone" {
			return errors.New("protocol 必须是 chat / messages / responses / systemone")
		}
		if m.Concurrency < 1 || m.Concurrency > 128 || m.RPM < 0 || m.RPM > 100000 {
			return errors.New("模型并发数 / RPM 不合法")
		}
		if m.MaxOutput < 1 || m.MaxOutput > 1000000 || m.Context < 1 || m.Context > 10000000 {
			return errors.New("上下文或输出限制不合法")
		}
		for _, v := range []float64{m.InputPrice, m.OutputPrice, m.CachePrice, m.WritePrice, m.Limit5h, m.Limit7d, m.Limit30d} {
			if v < 0 || v > 1000000 || math.IsNaN(v) || math.IsInf(v, 0) {
				return errors.New("价格与限额必须是有限非负数")
			}
		}
		if !m.PricingSet && (m.Limit5h > 0 || m.Limit7d > 0 || m.Limit30d > 0) {
			return errors.New("启用美元限额前必须确认模型计价")
		}
	}
	for _, r := range c.Routes {
		if !validID(r.ID) || names[r.ID] {
			return errors.New("路由 ID 无效或重复")
		}
		names[r.ID] = true
		if r.Strategy != "priority" && r.Strategy != "balanced" {
			return errors.New("路由策略仅支持 priority / balanced")
		}
		seen := map[string]bool{}
		for _, x := range r.Candidates {
			if _, ok := c.model(x.ModelID); !ok || seen[x.ModelID] {
				return errors.New("路由模型不存在或重复")
			}
			seen[x.ModelID] = true
			if x.Weight < 1 || x.Weight > 1000 {
				return errors.New("路由权重必须介于 1 和 1000")
			}
		}
	}
	for _, a := range c.Aliases {
		if !validID(a.ID) || names[a.ID] {
			return errors.New("别名 ID 无效或重复")
		}
		names[a.ID] = true
		if _, ok := c.model(a.Target); !ok {
			if _, ok = c.route(a.Target); !ok {
				return errors.New("别名只能指向已存在的模型或路由")
			}
		}
	}
	s := c.Settings
	if s.MaxBodyMB < 1 || s.MaxBodyMB > 32 || s.GlobalConcurrency < 1 || s.GlobalConcurrency > 512 || s.RetentionDays < 31 || s.RetentionDays > 3650 || s.SessionTTLHours < 1 || s.SessionTTLHours > 720 {
		return errors.New("设置范围：body 1–32MB，并发 1–512，保留 31–3650 天，会话 1–720 小时")
	}
	// 空值表示沿用默认，兼容升级前写入的旧配置
	if s.Listen != "" {
		if e := validateListenAddr(s.Listen); e != nil {
			return e
		}
	}
	switch strings.ToLower(s.LogLevel) {
	case "", "debug", "info", "warn", "error":
	default:
		return errors.New("日志级别必须是 debug / info / warn / error")
	}
	switch strings.ToLower(s.LogFormat) {
	case "", "text", "json":
	default:
		return errors.New("日志格式必须是 text / json")
	}
	return nil
}

type APIError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Status  int    `json:"-"`
}

func (e *APIError) Error() string             { return e.Message }
func fail(code, msg string, status int) error { return &APIError{code, msg, status} }

// nameTaken 报告某个对外 ID 是否已被模型、路由或别名占用——它们共用命名空间。
func (c Config) nameTaken(id string) bool {
	for _, m := range c.Models {
		if m.ID == id {
			return true
		}
	}
	for _, r := range c.Routes {
		if r.ID == id {
			return true
		}
	}
	for _, a := range c.Aliases {
		if a.ID == id {
			return true
		}
	}
	return false
}

// newSyncedModel 把上游 /models 里已经声明的能力抄下来。
// 抄的是事实（上下文、输出上限、工具、图像、单价），不做推断；
// pricing_set 一律保持 false：价格要由人对着自己的账户确认过才算数，
// 否则费用统计会拿着一个没人看过的数字装作确定。
func newSyncedModel(id, providerID, upstream, name, protocol string, src Object) Model {
	m := Model{
		ID: id, ProviderID: providerID, Upstream: upstream, Name: name,
		Protocol: protocol, Enabled: false, Concurrency: 2,
		Context: int(num(src, "context_length")),
	}
	if m.Context < 1 {
		m.Context = 128000
	}
	top := obj(src["top_provider"])
	m.MaxOutput = int(num(top, "max_completion_tokens"))
	if m.MaxOutput < 1 {
		// 上游没声明就按上下文估一个够用的值。
		// 原来固定 4096，同步来的模型连 Claude Code 的 64000 都满足不了。
		m.MaxOutput = min(m.Context, 32768)
	}
	m.MaxOutput = clamp(m.MaxOutput, 1, 1000000)
	m.Context = clamp(m.Context, 1, 10000000)

	for _, v := range arr(src["supported_parameters"]) {
		if s, _ := v.(string); s == "tools" {
			m.Tools = true
		}
	}
	for _, v := range arr(obj(src["architecture"])["input_modalities"]) {
		if s, _ := v.(string); s == "image" {
			m.Vision = true
		}
	}
	// 单价按「每百万 token 美元」存储，上游给的是每 token
	pricing := obj(src["pricing"])
	m.InputPrice = clampPrice(price(pricing, "prompt"))
	m.OutputPrice = clampPrice(price(pricing, "completion"))
	m.CachePrice = clampPrice(price(pricing, "input_cache_read"))
	m.WritePrice = clampPrice(price(pricing, "input_cache_write"))
	return m
}

func price(o Object, key string) float64 {
	switch v := o[key].(type) {
	case float64:
		return roundPrice(v * 1e6)
	case string:
		var f float64
		if _, err := fmt.Sscanf(v, "%g", &f); err == nil {
			return roundPrice(f * 1e6)
		}
	}
	return 0
}

// roundPrice 收敛浮点误差：每 token 单价乘以一百万后会得到
// 0.44999999999999996 这种值，直接存起来会一路显示到界面上。
// 保留六位小数，足以表达 $0.000001/M 的精度。
func roundPrice(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return math.Round(v*1e6) / 1e6
}

// clampPrice 把异常单价压回合法区间，避免一条畸形记录让整次同步因校验失败而回滚。
func clampPrice(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
		return 0
	}
	if v > 1000000 {
		return 1000000
	}
	return v
}
