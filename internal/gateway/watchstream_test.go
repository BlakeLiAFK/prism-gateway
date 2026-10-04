package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"prism-gateway/internal/webui"
)

// 缩短各个间隔，让需要等待的用例在一两秒内完成；清理顺序保证服务关闭后才还原
func shortWatchIntervals(t *testing.T) {
	t.Helper()
	oldTick, oldRecheck, oldQuiet := watchTick, watchRecheck, watchQuiet
	watchTick, watchRecheck, watchQuiet = 50*time.Millisecond, 50*time.Millisecond, 200*time.Millisecond
	t.Cleanup(func() { watchTick, watchRecheck, watchQuiet = oldTick, oldRecheck, oldQuiet })
}

func watchServer(t *testing.T, h *harness) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h.a)
	t.Cleanup(srv.Close)
	return srv
}

func openWatch(t *testing.T, srv *httptest.Server, token string, params Object) *http.Response {
	t.Helper()
	body := raw(Object{"action": "watch.stream", "params": params})
	req, err := http.NewRequest("POST", srv.URL+"/api.json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

type watchLines chan string

func readLines(body io.Reader) watchLines {
	ch := make(watchLines, 1024)
	go func() {
		defer close(ch)
		sc := bufio.NewScanner(body)
		sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
		for sc.Scan() {
			ch <- sc.Text()
		}
	}()
	return ch
}

func (w watchLines) waitFor(t *testing.T, want string, d time.Duration) string {
	t.Helper()
	deadline := time.After(d)
	var seen []string
	for {
		select {
		case l, ok := <-w:
			if !ok {
				t.Fatalf("流提前结束，没有等到 %q；已收到: %q", want, seen)
			}
			if seen = append(seen, l); strings.Contains(l, want) {
				return l
			}
		case <-deadline:
			t.Fatalf("等待 %q 超时；已收到: %q", want, seen)
		}
	}
}

func (w watchLines) waitEOF(t *testing.T, d time.Duration) []string {
	t.Helper()
	deadline := time.After(d)
	var seen []string
	for {
		select {
		case l, ok := <-w:
			if !ok {
				return seen
			}
			seen = append(seen, l)
		case <-deadline:
			t.Fatalf("流没有在 %v 内结束；已收到: %q", d, seen)
		}
	}
}

func newWatchToken(t *testing.T, h *harness) string {
	t.Helper()
	return str(createWatchToken(t, h, Object{}), "token")
}

func TestWatchStreamRejectsOtherCredentials(t *testing.T) {
	h := newHarness(t)
	srv := watchServer(t, h)
	token := newWatchToken(t, h)
	h.s.DB.Exec("UPDATE watch_tokens SET expires_at=?", now()-1000)
	for name, bad := range map[string]string{"管理员令牌": h.token, "网关 Key": h.createKey(t), "无": "", "已过期": token} {
		resp := openWatch(t, srv, bad, Object{"mode": "log"})
		var o Object
		json.NewDecoder(resp.Body).Decode(&o)
		if resp.StatusCode != 401 || str(obj(o["error"]), "code") != "UNAUTHORIZED" {
			t.Errorf("%s: status=%d body=%v", name, resp.StatusCode, o)
		}
		if strings.Contains(resp.Header.Get("Content-Type"), "text/plain") {
			t.Errorf("%s: 拒绝时不应进入推流", name)
		}
	}
}

// 监控令牌只能开监控流，调用任何其他管理 action 都是未登录
func TestWatchTokenCannotCallOtherActions(t *testing.T) {
	h := newHarness(t)
	token := newWatchToken(t, h)
	for _, action := range []string{"config.get", "watch.token_list", "apikey.list", "auth.status"} {
		w, o := h.rpc(t, action, Object{}, token)
		if action == "auth.status" {
			if obj(o["data"])["authenticated"] != false {
				t.Errorf("监控令牌不应被视为已登录: %v", o)
			}
			continue
		}
		if w.Code != 401 {
			t.Errorf("%s: 监控令牌应得 401，实得 %d", action, w.Code)
		}
	}
}

func TestWatchStreamValidatesParams(t *testing.T) {
	h := newHarness(t)
	srv := watchServer(t, h)
	token := newWatchToken(t, h)
	resp := openWatch(t, srv, token, Object{"mode": "bogus"})
	if resp.StatusCode != 400 {
		t.Fatalf("未知模式应返回 400: %d", resp.StatusCode)
	}
}

func TestWatchLogStreamHeadersBacklogAndLiveEvents(t *testing.T) {
	h := newHarness(t)
	srv := watchServer(t, h)
	token := newWatchToken(t, h)
	h.a.Engine.watch.publish("CONF", "v7  model.save before connect")
	resp := openWatch(t, srv, token, Object{"mode": "log"})
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/plain") {
		t.Fatalf("status=%d type=%q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	if cc := resp.Header.Get("Cache-Control"); !strings.Contains(cc, "no-cache") || !strings.Contains(cc, "no-transform") {
		t.Errorf("Cache-Control = %q", cc)
	}
	if resp.Header.Get("X-Accel-Buffering") != "no" {
		t.Error("需要 X-Accel-Buffering: no，反向代理才不会缓冲")
	}
	lines := readLines(resp.Body)
	lines.waitFor(t, "prism gateway "+Version, 2*time.Second)
	lines.waitFor(t, "model.save before connect", 2*time.Second) // 连接前发生的事也能回看
	h.a.Engine.watch.publish("ALERT", "route auto has no candidate")
	l := lines.waitFor(t, "route auto has no candidate", 2*time.Second)
	if !strings.Contains(l, "ALERT") {
		t.Errorf("事件行缺少类型: %q", l)
	}
	if rows, _ := h.s.DB.Query("SELECT last_used_at FROM watch_tokens"); len(rows) != 1 || rows[0].Int("last_used_at") == 0 {
		t.Error("连接后应记录令牌最近使用时间")
	}
}

func TestWatchLogStreamNeverEmitsTerminalEscapes(t *testing.T) {
	h := newHarness(t)
	srv := watchServer(t, h)
	resp := openWatch(t, srv, newWatchToken(t, h), Object{"mode": "log", "color": false})
	lines := readLines(resp.Body)
	lines.waitFor(t, "prism gateway", 2*time.Second)
	h.a.Engine.watch.publish("REJ", "x\x1b[2J\x1b]0;pwned\x07y")
	l := lines.waitFor(t, "x", 2*time.Second)
	if strings.ContainsAny(l, "\x1b\x07") {
		t.Fatalf("事件行带出了终端控制字符: %q", l)
	}
}

func TestWatchLogStreamHeartbeat(t *testing.T) {
	shortWatchIntervals(t)
	h := newHarness(t)
	srv := watchServer(t, h)
	resp := openWatch(t, srv, newWatchToken(t, h), Object{"mode": "log"})
	lines := readLines(resp.Body)
	lines.waitFor(t, "alive", 3*time.Second)
}

func TestWatchDashboardStream(t *testing.T) {
	h := newHarness(t)
	h.configure(t, "http://127.0.0.1:1", modelFixture("m", "chat"))
	h.change(t, func(c *Config) {
		c.Routes = []Route{{ID: "auto", Name: "auto", Enabled: true, Strategy: "priority", Candidates: []Candidate{{ModelID: "m", Weight: 10}}}}
	})
	srv := watchServer(t, h)
	resp := openWatch(t, srv, newWatchToken(t, h), Object{"mode": "dashboard", "width": 80, "height": 30})
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	lines := readLines(resp.Body)
	first := lines.waitFor(t, "PRISM GATEWAY "+Version, 3*time.Second)
	if !strings.Contains(first, "\x1b[2J") && !strings.Contains(first, "\x1b[H") {
		t.Errorf("第一帧应先清屏并回到左上角: %q", first)
	}
	lines.waitFor(t, "ROUTE auto", 2*time.Second)
	lines.waitFor(t, "EVENTS", 2*time.Second)
	for i := 0; i < 12; i++ {
		l := <-lines
		if tw := textWidth(stripANSI(l)); tw > 80 {
			t.Fatalf("仪表盘行宽 %d 超出请求的 80 列: %q", tw, stripANSI(l))
		}
	}
}

func TestWatchStreamEndsWhenTokenRevoked(t *testing.T) {
	shortWatchIntervals(t)
	h := newHarness(t)
	srv := watchServer(t, h)
	token := newWatchToken(t, h)
	resp := openWatch(t, srv, token, Object{"mode": "log"})
	lines := readLines(resp.Body)
	lines.waitFor(t, "prism gateway", 2*time.Second)
	id, _, _ := h.a.checkWatchToken(token)
	if w, o := h.rpc(t, "watch.token_revoke", Object{"id": id}, h.token); w.Code != 200 {
		t.Fatalf("撤销失败: %v", o)
	}
	rest := lines.waitEOF(t, 3*time.Second)
	if !strings.Contains(strings.Join(rest, "\n"), "revoked") {
		t.Errorf("撤销后应先说明原因再结束: %q", rest)
	}
}

func TestWatchStreamEndsWhenServerStops(t *testing.T) {
	h := newHarness(t)
	srv := watchServer(t, h)
	resp := openWatch(t, srv, newWatchToken(t, h), Object{"mode": "log"})
	lines := readLines(resp.Body)
	lines.waitFor(t, "prism gateway", 2*time.Second)
	h.a.Engine.watch.closeAll()
	rest := lines.waitEOF(t, 2*time.Second)
	if !strings.Contains(strings.Join(rest, "\n"), "restarting") {
		t.Errorf("停止时应提示即将重连: %q", rest)
	}
	// 关闭之后不再接受新连接，客户端据此知道要稍后重试
	if resp := openWatch(t, srv, newWatchToken(t, h), Object{"mode": "log"}); resp.StatusCode != 503 {
		t.Errorf("关闭后新连接应得 503: %d", resp.StatusCode)
	}
}

func TestWatchStreamLimitsConnectionsPerToken(t *testing.T) {
	h := newHarness(t)
	srv := watchServer(t, h)
	token := newWatchToken(t, h)
	for i := 0; i < watchPerToken; i++ {
		resp := openWatch(t, srv, token, Object{"mode": "log"})
		readLines(resp.Body).waitFor(t, "prism gateway", 2*time.Second)
	}
	if resp := openWatch(t, srv, token, Object{"mode": "log"}); resp.StatusCode != 429 {
		t.Fatalf("超过单令牌上限应得 429: %d", resp.StatusCode)
	}
	// 另一个令牌不受影响
	if resp := openWatch(t, srv, newWatchToken(t, h), Object{"mode": "log"}); resp.StatusCode != 200 {
		t.Fatalf("其他令牌应正常: %d", resp.StatusCode)
	}
}

// 网关自己停止时（进程收到信号，上下文取消），监控流要立刻结束，不能占着优雅停止的等待时间
func TestWatchStreamsEndWhenAppContextIsCanceled(t *testing.T) {
	s, err := OpenStore(filepath.Join(t.TempDir(), "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	a := NewApp(ctx, s, webui.Handler())
	t.Cleanup(func() { cancel(); a.Wait(); s.DB.Close() })
	made, err := a.createWatchToken(Object{})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(a)
	t.Cleanup(srv.Close)
	resp := openWatch(t, srv, str(made.(Object), "token"), Object{"mode": "log"})
	lines := readLines(resp.Body)
	lines.waitFor(t, "prism gateway", 2*time.Second)
	cancel()
	lines.waitEOF(t, 3*time.Second)
}

// 反向代理后面所有客户端的直连地址都是同一个，监控流的失败尝试不能占用登录的限频额度，
// 否则任何人乱发监控令牌都能把管理员锁在后台之外
func TestWatchBadTokenAttemptsDoNotLockAdminLogin(t *testing.T) {
	h := newHarness(t)
	attempt := func(action string, params Object, token string) int {
		r := httptest.NewRequest("POST", "http://localhost/api.json", strings.NewReader(raw(Object{"action": action, "params": params})))
		r.Header.Set("Content-Type", "application/json")
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		h.a.ServeHTTP(w, r)
		return w.Code
	}
	var limited int
	for i := 0; i < 30; i++ {
		if attempt("watch.stream", Object{"mode": "log"}, "prism_watch_"+strings.Repeat("x", 40)) == 429 {
			limited++
		}
	}
	if limited == 0 {
		t.Fatal("监控令牌的失败尝试本身也应当被限频")
	}
	if code := attempt("auth.login", Object{"token": h.token}, ""); code != 200 {
		t.Fatalf("监控流的失败尝试不应影响管理员登录: %d", code)
	}
}
