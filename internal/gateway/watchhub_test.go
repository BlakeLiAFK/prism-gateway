package gateway

import (
	"strings"
	"testing"
	"time"
)

func TestWatchHubBacklogKeepsNewestInOrder(t *testing.T) {
	h := newWatchHub()
	for i := 0; i < watchRing+30; i++ {
		h.publish("OK", strings.Repeat("x", i%3)+"#"+string(rune('a'+i%26)))
	}
	got := h.recent(5)
	if len(got) != 5 {
		t.Fatalf("回看条数 = %d", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i].At < got[i-1].At {
			t.Fatal("回看必须按时间从旧到新")
		}
	}
	if all := h.recent(10000); len(all) != watchRing {
		t.Fatalf("环形缓冲只保留最近 %d 条，实得 %d", watchRing, len(all))
	}
}

// 订阅者读不过来时，发布方不能被拖慢：缓冲满了丢事件并计数
func TestWatchHubSlowSubscriberNeverBlocksPublisher(t *testing.T) {
	h := newWatchHub()
	sub, _, err := h.subscribe("tok")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		for i := 0; i < watchSubBuffer*4; i++ {
			h.publish("OK", "event")
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("订阅者不读取时发布方被阻塞")
	}
	if n := h.takeDropped(sub); n != watchSubBuffer*3 {
		t.Fatalf("丢弃计数 = %d，期望 %d", n, watchSubBuffer*3)
	}
	if n := h.takeDropped(sub); n != 0 {
		t.Fatalf("取走之后应清零: %d", n)
	}
}

// 事件文本里有来自客户端的字符串，终端转义序列不能原样送到管理员的终端
func TestWatchHubStripsTerminalControls(t *testing.T) {
	h := newWatchHub()
	h.publish("REJ", "model\x1b[31mRED\x1b]0;title\x07 line\nbreak\u009b\u202eevil")
	text := h.recent(1)[0].Text
	for _, r := range text {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) || r == 0x202e {
			t.Fatalf("残留控制字符 %U: %q", r, text)
		}
	}
	if !strings.Contains(text, "RED") || !strings.Contains(text, "line break") {
		t.Fatalf("可见内容不应丢失，换行应变成空格: %q", text)
	}
}

func TestWatchHubBoundsTextLength(t *testing.T) {
	h := newWatchHub()
	h.publish("ALERT", strings.Repeat("长", 5000))
	if n := len([]rune(h.recent(1)[0].Text)); n > watchTextMax+1 {
		t.Fatalf("文本长度 = %d", n)
	}
}

func TestWatchHubLimitsStreams(t *testing.T) {
	h := newWatchHub()
	var subs []*watchSub
	for i := 0; i < watchPerToken; i++ {
		s, _, err := h.subscribe("a")
		if err != nil {
			t.Fatalf("第 %d 条应成功: %v", i+1, err)
		}
		subs = append(subs, s)
	}
	if _, _, err := h.subscribe("a"); err == nil {
		t.Fatal("同一令牌超过上限应被拒绝")
	}
	h.unsubscribe(subs[0])
	if _, _, err := h.subscribe("a"); err != nil {
		t.Fatalf("释放一条后应能再连: %v", err)
	}
	for i := 0; i < watchMaxStreams; i++ {
		if _, _, err := h.subscribe(string(rune('b' + i))); err != nil {
			break
		}
	}
	if _, _, err := h.subscribe("zz"); err == nil {
		t.Fatal("全局连接数达到上限后应拒绝新连接")
	}
	if h.watchers() > watchMaxStreams {
		t.Fatalf("连接数 %d 超过上限", h.watchers())
	}
}

func TestWatchHubSeriesPerMinute(t *testing.T) {
	h := newWatchHub()
	h.countRequest(false)
	h.countRequest(true)
	h.countRequest(false)
	req, errs := h.series()
	if len(req) != watchMinutes || len(errs) != watchMinutes {
		t.Fatalf("序列长度 = %d/%d", len(req), len(errs))
	}
	// 最后一格是当前分钟；个别情况下测试正好跨过整分钟，所以取两格之和
	last := req[watchMinutes-1] + req[watchMinutes-2]
	lastErr := errs[watchMinutes-1] + errs[watchMinutes-2]
	if last != 3 || lastErr != 1 {
		t.Fatalf("请求 %d、失败 %d，期望 3 和 1", last, lastErr)
	}
}

func TestWatchHubCloseEndsStreamsAndRefusesNewOnes(t *testing.T) {
	h := newWatchHub()
	if _, _, err := h.subscribe("a"); err != nil {
		t.Fatal(err)
	}
	h.closeAll()
	select {
	case <-h.done:
	default:
		t.Fatal("关闭后 done 应已关闭")
	}
	if _, _, err := h.subscribe("a"); err == nil {
		t.Fatal("关闭后不应再接受订阅")
	}
	h.closeAll() // 重复关闭不能 panic
}
