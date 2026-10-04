package gateway

import (
	"strings"
	"testing"
	"time"
)

func createWatchToken(t *testing.T, h *harness, params Object) Object {
	t.Helper()
	w, o := h.rpc(t, "watch.token_create", params, h.token)
	if w.Code != 200 {
		t.Fatalf("创建监控令牌失败: %d %v", w.Code, o)
	}
	return obj(o["data"])
}

func TestWatchTokenLifecycle(t *testing.T) {
	h := newHarness(t)
	d := createWatchToken(t, h, Object{"name": "laptop"})
	token := str(d, "token")
	if !strings.HasPrefix(token, "prism_watch_") || len(token) < 40 {
		t.Fatalf("令牌格式不对: %q", token)
	}
	// 默认 7 天
	if left := time.Duration(int64(num(d, "expires_at"))-now()) * time.Millisecond; left < 6*24*time.Hour+23*time.Hour || left > 7*24*time.Hour {
		t.Fatalf("默认有效期应为 7 天，实得 %v", left)
	}
	id, _, ok := h.a.checkWatchToken(token)
	if !ok || id != str(d, "id") {
		t.Fatalf("新令牌应当有效: id=%q ok=%t", id, ok)
	}

	// 列表不含明文和摘要，只给界面需要的字段
	_, list := h.rpc(t, "watch.token_list", Object{}, h.token)
	items := arr(list["data"])
	if len(items) != 1 || str(obj(items[0]), "name") != "laptop" {
		t.Fatalf("列表内容不对: %v", list)
	}
	if body := raw(list["data"]); strings.Contains(body, token) || strings.Contains(body, digest(token)) || strings.Contains(body, "digest") {
		t.Fatalf("列表泄露了令牌或摘要: %s", body)
	}

	// 库里只存摘要
	rows, err := h.s.DB.Query("SELECT * FROM watch_tokens")
	if err != nil || len(rows) != 1 || strings.Contains(raw(rows), token) {
		t.Fatalf("库里不应出现明文令牌: %d %v %v", len(rows), err, raw(rows))
	}

	// 撤销后立即失效，并从列表消失
	if w, o := h.rpc(t, "watch.token_revoke", Object{"id": id}, h.token); w.Code != 200 {
		t.Fatalf("撤销失败: %v", o)
	}
	if _, _, ok := h.a.checkWatchToken(token); ok {
		t.Fatal("撤销后令牌必须失效")
	}
	_, list = h.rpc(t, "watch.token_list", Object{}, h.token)
	if len(arr(list["data"])) != 0 {
		t.Fatalf("撤销后不应再列出: %v", list)
	}
	// 审计里要有创建与撤销，但不含令牌
	logs, _ := h.s.DB.Query("SELECT action,target FROM audit_logs WHERE action LIKE 'watch.%' ORDER BY created_at")
	if len(logs) != 2 || strings.Contains(raw(logs), token) {
		t.Fatalf("审计记录不对: %v", raw(logs))
	}
}

func TestWatchTokenRejectsOtherCredentials(t *testing.T) {
	h := newHarness(t)
	token := str(createWatchToken(t, h, Object{}), "token")
	for name, bad := range map[string]string{
		"管理员令牌":  h.token,
		"网关 Key": h.createKey(t),
		"截断":     token[:30],
		"空":      "",
		"相近":     token[:len(token)-1] + map[bool]string{true: "y", false: "x"}[strings.HasSuffix(token, "x")],
	} {
		if _, _, ok := h.a.checkWatchToken(bad); ok {
			t.Errorf("%s 不应被当作监控令牌", name)
		}
	}
}

func TestWatchTokenExpiryAndValidation(t *testing.T) {
	h := newHarness(t)
	token := str(createWatchToken(t, h, Object{"days": 1}), "token")
	if err := h.s.DB.Exec("UPDATE watch_tokens SET expires_at=?", now()-1000); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := h.a.checkWatchToken(token); ok {
		t.Fatal("过期令牌必须失效")
	}
	for _, days := range []any{0.0, -3.0, 31.0, 400.0} {
		w, o := h.rpc(t, "watch.token_create", Object{"days": days}, h.token)
		if days == 0.0 {
			if w.Code != 200 {
				t.Errorf("days=0 应取默认值: %v", o)
			}
			continue
		}
		if w.Code != 400 {
			t.Errorf("days=%v 应被拒绝: %d", days, w.Code)
		}
	}
	if w, _ := h.rpc(t, "watch.token_create", Object{"name": strings.Repeat("n", 200)}, h.token); w.Code != 400 {
		t.Errorf("名称过长应被拒绝: %d", w.Code)
	}
}

func TestWatchTokenLimitAndAuth(t *testing.T) {
	h := newHarness(t)
	for i := 0; i < watchTokenMax; i++ {
		createWatchToken(t, h, Object{})
	}
	if w, _ := h.rpc(t, "watch.token_create", Object{}, h.token); w.Code != 400 {
		t.Fatalf("有效令牌达到上限后应拒绝创建: %d", w.Code)
	}
	// 管理动作必须登录；监控令牌自己不能管理令牌
	token := str(obj(func() Object {
		_, o := h.rpc(t, "watch.token_list", Object{}, h.token)
		return Object{"x": arr(o["data"])[0]}
	}()["x"]), "id")
	if w, _ := h.rpc(t, "watch.token_list", Object{}, ""); w.Code != 401 {
		t.Errorf("未登录应返回 401: %d", w.Code)
	}
	if w, _ := h.rpc(t, "watch.token_revoke", Object{"id": token}, ""); w.Code != 401 {
		t.Errorf("未登录不能撤销: %d", w.Code)
	}
	if w, _ := h.rpc(t, "watch.token_revoke", Object{"id": "nope"}, h.token); w.Code != 404 {
		t.Errorf("撤销不存在的令牌应返回 404: %d", w.Code)
	}
}

func TestWatchTokenPrunedAfterExpiry(t *testing.T) {
	h := newHarness(t)
	createWatchToken(t, h, Object{})
	createWatchToken(t, h, Object{})
	if err := h.s.DB.Exec("UPDATE watch_tokens SET expires_at=? WHERE id=(SELECT id FROM watch_tokens LIMIT 1)", now()-2*86400000); err != nil {
		t.Fatal(err)
	}
	h.a.Engine.pruneOnce()
	rows, _ := h.s.DB.Query("SELECT id FROM watch_tokens")
	if len(rows) != 1 {
		t.Fatalf("过期超过一天的令牌应被清理，剩 %d 条", len(rows))
	}
}
