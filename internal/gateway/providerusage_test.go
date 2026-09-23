package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// admin key 只用于查官方账单：保存后不回显、导入保留、重启仍在，
// 查询时换成 admin key 调账单接口并把各天金额加总。
func TestAdminKeyBilling(t *testing.T) {
	h := newHarness(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/organization/costs":
			if r.Header.Get("Authorization") != "Bearer sk-admin-oa" || r.URL.Query().Get("start_time") == "" {
				w.WriteHeader(401)
				return
			}
			io.WriteString(w, `{"object":"page","data":[{"results":[{"amount":{"value":1.25,"currency":"usd"}}]},{"results":[{"amount":{"value":2.5,"currency":"usd"}}]},{"results":[]}],"has_more":false}`)
		case "/v1/organizations/cost_report":
			if r.Header.Get("x-api-key") != "sk-admin-an" || r.Header.Get("anthropic-version") == "" || r.URL.Query().Get("starting_at") == "" {
				w.WriteHeader(401)
				return
			}
			// Anthropic 的金额以美分计
			io.WriteString(w, `{"data":[{"results":[{"amount":"123.45","currency":"USD"}]},{"results":[{"amount":"100","currency":"USD"}]}],"has_more":false}`)
		default:
			w.WriteHeader(404)
		}
	}))
	defer upstream.Close()
	h.change(t, func(c *Config) {
		c.Providers = append(c.Providers,
			Provider{ID: "p_oa", Name: "OpenAI", Kind: "openai", Auth: "auto", Secret: "sk-infer", BaseURL: upstream.URL + "/v1", AllowPrivate: true, Enabled: true, TimeoutSec: 30},
			Provider{ID: "p_an", Name: "Anthropic", Kind: "anthropic", Auth: "auto", Secret: "sk-infer", BaseURL: upstream.URL + "/v1", AllowPrivate: true, Enabled: true, TimeoutSec: 30},
			Provider{ID: "p_oa2", Name: "OpenAI 无 admin", Kind: "openai", Auth: "auto", Secret: "sk-infer", BaseURL: upstream.URL + "/v1", AllowPrivate: true, Enabled: true, TimeoutSec: 30})
		c.Models = append(c.Models, modelFixture("m_oa", "chat"))
		c.Models[len(c.Models)-1].ProviderID = "p_oa2"
	})
	for id, key := range map[string]string{"p_oa": "sk-admin-oa", "p_an": "sk-admin-an"} {
		w, o := h.rpc(t, "provider.save", Object{"version": h.s.Config().Version, "id": id, "provider": Object{"admin_key": key}}, h.token)
		if w.Code != 200 || o["ok"] != true {
			t.Fatalf("保存 admin key 失败: %s", w.Body)
		}
		if strings.Contains(w.Body.String(), key) {
			t.Fatal("保存结果回显了 admin key 明文")
		}
	}
	if p, _ := h.s.Config().provider("p_oa"); !p.HasAdminKey || p.Secret != "sk-infer" {
		t.Fatalf("admin key 未保存或误改了推理凭证: %+v", p)
	}

	// 上游在响应头里声明过限额的模型，限额快照要挂到所属供应商上
	h.a.Engine.recordLimits("m_oa", http.Header{"X-Ratelimit-Remaining-Requests": {"42"}})

	_, o := h.rpc(t, "provider.usage", Object{}, h.token)
	got := map[string]Object{}
	for _, v := range arr(o["data"]) {
		got[str(obj(v), "id")] = obj(v)
	}
	if str(got["p_oa"], "headline") != "本月 $3.75" {
		t.Fatalf("OpenAI 本月花费不正确: %v", got["p_oa"])
	}
	if str(got["p_an"], "headline") != "本月 $2.23" {
		t.Fatalf("Anthropic 本月花费不正确（美分换算）: %v", got["p_an"])
	}
	oa2 := got["p_oa2"]
	if oa2["supported"] != false {
		t.Fatalf("没有 admin key 时应标注不支持: %v", oa2)
	}
	limits := arr(oa2["limits"])
	if len(limits) != 1 || str(obj(limits[0]), "model") != "m_oa" || str(obj(obj(limits[0])["limits"]), "x-ratelimit-remaining-requests") != "42" {
		t.Fatalf("供应商缺少上游限额快照: %v", oa2)
	}

	// 清掉 admin key 后要立即生效，不能被 60 秒缓存挡住
	h.rpc(t, "provider.save", Object{"version": h.s.Config().Version, "id": "p_oa", "provider": Object{"admin_key": ""}}, h.token)
	_, o = h.rpc(t, "provider.usage", Object{}, h.token)
	if u := obj(arr(o["data"])[0]); str(u, "id") != "p_oa" || u["supported"] != false {
		t.Fatalf("清除 admin key 后仍返回缓存的花费: %v", u)
	}
	h.rpc(t, "provider.save", Object{"version": h.s.Config().Version, "id": "p_oa", "provider": Object{"admin_key": "sk-admin-oa"}}, h.token)

	// 导入不能丢掉库里的 admin key；导出不能含明文
	w, o := h.rpc(t, "config.export", Object{}, h.token)
	if strings.Contains(w.Body.String(), "sk-admin-") {
		t.Fatal("导出包含 admin key 明文")
	}
	h.rpc(t, "config.import", Object{"version": h.s.Config().Version, "format": "prism.config/1", "config": obj(obj(o["data"])["config"])}, h.token)
	if p, _ := h.s.Config().provider("p_an"); p.AdminSecret != "sk-admin-an" {
		t.Fatal("导入丢失了 admin key")
	}

	// 老库没有 admin_secret 列：重开时自动补列，已有凭证不受影响
	dbPath := strings.TrimSuffix(h.s.KeyPath, ".key")
	h.s.DB.Exec("UPDATE providers SET admin_secret=''")
	if err := h.s.DB.Exec("ALTER TABLE providers DROP COLUMN admin_secret"); err != nil {
		t.Fatal(err)
	}
	h.s.DB.Close()
	s2, err := OpenStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.DB.Close()
	if p, _ := s2.Config().provider("p_oa"); p.Secret != "sk-infer" || p.HasAdminKey {
		t.Fatalf("老库补列后凭证不正确: %+v", p)
	}
}
