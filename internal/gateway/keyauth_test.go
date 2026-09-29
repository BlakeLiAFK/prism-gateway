package gateway

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"prism-gateway/internal/sqlite"
)

func keyRequest(key string) *http.Request {
	r := httptest.NewRequest("POST", "/openai/v1/chat/completions", nil)
	r.Header.Set("Authorization", "Bearer "+key)
	return r
}

// holdDB 占住数据库连接，直到返回的函数被调用
func holdDB(t *testing.T, h *harness) (release func()) {
	t.Helper()
	held, hold := make(chan struct{}), make(chan struct{})
	go h.s.DB.Transaction(func(*sqlite.Tx) error {
		close(held)
		<-hold
		return nil
	})
	<-held
	return func() { close(hold) }
}

// within 在限定时间内运行 fn，超时说明它被阻塞了
func within(t *testing.T, d time.Duration, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { fn(); close(done) }()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("%s 被阻塞", what)
	}
}

// 快照里没有的 Key 直接判无效，不碰数据库：随机字符串的无效 Key 刷不到数据库上，
// 数据库被占住时，快照里已有的 Key 也照常通过。
func TestUnknownKeyNeverTouchesDatabase(t *testing.T) {
	h := newHarness(t)
	good := h.createKey(t)
	e := h.a.Engine
	// 预热：载入快照，并写过 last_used，之后一分钟内不会再写库
	if _, err := e.Authenticate(keyRequest(good)); err != nil {
		t.Fatal(err)
	}
	release := holdDB(t, h)
	defer release()
	within(t, 2*time.Second, "无效 Key 的判定", func() {
		for i := 0; i < 200; i++ {
			if status, _, _ := errorParts(func() error {
				_, err := e.Authenticate(keyRequest(fmt.Sprintf("sk-invalid-%016d", i)))
				return err
			}()); status != 401 {
				t.Errorf("无效 Key 应为 401，实得 %d", status)
				return
			}
		}
	})
	within(t, 2*time.Second, "快照内的有效 Key", func() {
		if _, err := e.Authenticate(keyRequest(good)); err != nil {
			t.Error(err)
		}
	})
}

// 快照足够新时，别的进程刚写入的 Key 最多晚一个刷新周期生效，之后被找到。
func TestSnapshotRefreshFindsKeyCreatedElsewhere(t *testing.T) {
	h := newHarness(t)
	e := h.a.Engine
	if _, err := e.Authenticate(keyRequest(h.createKey(t))); err != nil {
		t.Fatal(err)
	}
	token := "prism_sk_created-by-another-process"
	err := h.s.DB.Exec("INSERT INTO api_keys(id,name,prefix,digest,enabled,allowed,created_at,expires_at,limit_day,limit_month,rpm) VALUES ('key_x','x','prism_sk_created-b',?,1,'[]',?,0,0,0,0)", digest(token), now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Authenticate(keyRequest(token)); err == nil {
		t.Fatal("快照仍新时不应重载")
	}
	e.keyMu.Lock()
	e.keysAt = now() - keyRefreshEvery - 1
	e.keyMu.Unlock()
	if _, err := e.Authenticate(keyRequest(token)); err != nil {
		t.Fatalf("快照过期后应重载并找到新 Key: %v", err)
	}
}

// 本进程内创建与吊销 Key 必须立即生效，不能等刷新周期。
func TestKeyCreateAndRevokeTakeEffectImmediately(t *testing.T) {
	h := newHarness(t)
	e := h.a.Engine
	first := h.createKey(t)
	if _, err := e.Authenticate(keyRequest(first)); err != nil {
		t.Fatal(err)
	}
	second := h.createKey(t) // 快照刚载入过，创建后必须立刻可用
	if _, err := e.Authenticate(keyRequest(second)); err != nil {
		t.Fatalf("新建的 Key 应立即可用: %v", err)
	}
	_, list := h.rpc(t, "apikey.list", Object{}, h.token)
	var id string
	for _, v := range arr(list["data"]) {
		if str(obj(v), "prefix") == first[:17] {
			id = str(obj(v), "id")
		}
	}
	if id == "" {
		t.Fatalf("找不到第一把 Key: %v", list)
	}
	if w, _ := h.rpc(t, "apikey.revoke", Object{"id": id}, h.token); w.Code != 200 {
		t.Fatalf("吊销失败: %d %s", w.Code, w.Body)
	}
	if status, _, _ := errorParts(func() error { _, err := e.Authenticate(keyRequest(first)); return err }()); status != 401 {
		t.Fatalf("吊销后应立即 401，实得 %d", status)
	}
	if _, err := e.Authenticate(keyRequest(second)); err != nil {
		t.Fatalf("其他 Key 不应受影响: %v", err)
	}
}

// 快照重载卡在数据库上时，快照里已有的 Key 不能跟着排队。
// 用 onKeyLoad 钩子确认重载已经开始，不靠 sleep 猜时序。
func TestSnapshotHitNotBlockedByReloadInFlight(t *testing.T) {
	h := newHarness(t)
	e := h.a.Engine
	good := h.createKey(t)
	if _, err := e.Authenticate(keyRequest(good)); err != nil {
		t.Fatal(err)
	}
	e.keyMu.Lock()
	e.keysAt = now() - keyRefreshEvery - 1 // 让下一个未知 Key 触发重载
	e.keyMu.Unlock()
	reached := make(chan struct{})
	e.onKeyLoad = func() { close(reached) }
	release := holdDB(t, h)
	defer release()
	go e.Authenticate(keyRequest("sk-unknown-token-000000001"))
	<-reached
	within(t, 2*time.Second, "快照内的 Key", func() {
		if _, err := e.Authenticate(keyRequest(good)); err != nil {
			t.Error(err)
		}
	})
}

// 重载读库期间快照被作废（例如刚吊销了 Key）：这次结果只用于当前请求，不能装成新快照。
// 钩子在读库前同步触发 forgetKeys，代际变化是确定的。
func TestReloadDoesNotInstallSnapshotAcrossForget(t *testing.T) {
	h := newHarness(t)
	e := h.a.Engine
	good := h.createKey(t)
	e.onKeyLoad = e.forgetKeys
	if _, err := e.Authenticate(keyRequest(good)); err != nil {
		t.Fatalf("本次请求仍应按读到的结果通过: %v", err)
	}
	e.keyMu.Lock()
	at := e.keysAt
	e.keyMu.Unlock()
	if at != 0 {
		t.Fatal("代际变化后的读库结果不应作为快照")
	}
}

// 一条损坏的 Key 记录只让这把 Key 不可用，不能连累其他 Key。
func TestCorruptKeyRecordDoesNotBreakOthers(t *testing.T) {
	h := newHarness(t)
	e := h.a.Engine
	good := h.createKey(t)
	bad := "prism_sk_corrupt-record-token"
	err := h.s.DB.Exec("INSERT INTO api_keys(id,name,prefix,digest,enabled,allowed,created_at,expires_at,limit_day,limit_month,rpm) VALUES ('key_bad','bad','prism_sk_corrupt-r',?,1,'not json',?,0,0,0,0)", digest(bad), now())
	if err != nil {
		t.Fatal(err)
	}
	e.forgetKeys()
	if _, err := e.Authenticate(keyRequest(good)); err != nil {
		t.Fatalf("其他 Key 不应受损坏记录影响: %v", err)
	}
	if status, _, _ := errorParts(func() error { _, err := e.Authenticate(keyRequest(bad)); return err }()); status != 401 {
		t.Fatalf("损坏记录对应的 Key 应为 401，实得 %d", status)
	}
}

// 管理端登录后取得会话 Cookie 与 CSRF
func adminLogin(t *testing.T, h *harness) (*http.Cookie, string) {
	t.Helper()
	w, o := h.rpc(t, "auth.login", Object{"token": h.token}, "")
	requireStatus(t, w, 200)
	return w.Result().Cookies()[0], str(obj(o["data"]), "csrf")
}

func adminCall(h *harness, action string, cookie *http.Cookie, csrf string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "http://localhost/api.json", strings.NewReader(raw(Object{"action": action, "params": Object{}})))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Prism-CSRF", csrf)
	r.AddCookie(cookie)
	w := httptest.NewRecorder()
	h.a.ServeHTTP(w, r)
	return w
}

// 退出登录后，原会话 Cookie 必须真的失效。
func TestLogoutInvalidatesSession(t *testing.T) {
	h := newHarness(t)
	cookie, csrf := adminLogin(t, h)
	requireStatus(t, adminCall(h, "config.get", cookie, csrf), 200)
	requireStatus(t, adminCall(h, "auth.logout", cookie, csrf), 200)
	requireStatus(t, adminCall(h, "config.get", cookie, csrf), 401)
}

// 会话删除失败时必须报错，不能告诉界面「已退出」而库里的会话仍然有效。
func TestLogoutReportsSessionDeleteFailure(t *testing.T) {
	h := newHarness(t)
	cookie, csrf := adminLogin(t, h)
	// 让删除语句失败：换成一张同名视图（对视图 DELETE 会报错）
	for _, q := range []string{"ALTER TABLE admin_sessions RENAME TO admin_sessions_real", "CREATE VIEW admin_sessions AS SELECT * FROM admin_sessions_real"} {
		if err := h.s.DB.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	w := adminCall(h, "auth.logout", cookie, csrf)
	if w.Code != 500 {
		t.Fatalf("删除失败应返回 500，实得 %d %s", w.Code, w.Body)
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == "prism_session" && c.MaxAge < 0 {
			t.Fatal("删除失败时不应清除客户端 Cookie")
		}
	}
}
