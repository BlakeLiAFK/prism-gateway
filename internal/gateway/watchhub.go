package gateway

// 终端监控的事件中心：把引擎各处发生的事广播给正在看监控流的订阅者。
// 两条原则：
//  1. 发布方永远不能被订阅者拖慢。订阅者缓冲满了就丢事件并计数，绝不阻塞请求路径。
//  2. 事件只存内存、只含元数据（模型、耗时、错误码），不含提示词、回答和密钥。
// 事件文本里有来自客户端或上游的字符串，进入中心前统一清洗控制字符：
// 否则一个带转义序列的 model 名就能在管理员的终端里画出任意内容。

import (
	"strings"
	"sync"
)

const (
	watchRing       = 200 // 保留最近事件，新连接回看
	watchMinutes    = 30  // 趋势图覆盖的分钟数
	watchSubBuffer  = 128 // 每个订阅者的缓冲
	watchMaxStreams = 16  // 全局同时连接上限
	watchPerToken   = 3   // 单个令牌同时连接上限
	watchTextMax    = 300 // 单条事件文本的字符上限
)

type watchEvent struct {
	At   int64
	Kind string
	Text string
}

type watchBucket struct{ minute, reqs, errs int64 }

type watchSub struct {
	ch      chan watchEvent
	token   string
	dropped int64 // 受 watchHub.mu 保护
}

type watchHub struct {
	mu      sync.Mutex
	subs    map[*watchSub]struct{}
	perTok  map[string]int
	ring    [watchRing]watchEvent
	total   int
	buckets [watchMinutes]watchBucket
	done    chan struct{}
	once    sync.Once
}

func newWatchHub() *watchHub {
	return &watchHub{subs: map[*watchSub]struct{}{}, perTok: map[string]int{}, done: make(chan struct{})}
}

// cleanText 把控制字符、C1 控制符、双向覆盖符换成安全字符。换行和制表符变空格，保持单行。
func cleanText(s string) string {
	out := strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\r' || r == '\t':
			return ' '
		case r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f),
			r >= 0x200b && r <= 0x200f, r >= 0x2028 && r <= 0x202e, r >= 0x2066 && r <= 0x2069:
			return '?'
		}
		return r
	}, s)
	if rs := []rune(out); len(rs) > watchTextMax {
		out = string(rs[:watchTextMax]) + "…"
	}
	return out
}

// publish 记入回看缓冲并广播给所有订阅者。缓冲满的订阅者丢弃这一条。
func (h *watchHub) publish(kind, text string) {
	ev := watchEvent{At: now(), Kind: kind, Text: cleanText(text)}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.ring[h.total%watchRing] = ev
	h.total++
	for s := range h.subs {
		select {
		case s.ch <- ev:
		default:
			s.dropped++
		}
	}
}

// countRequest 记一次请求结果，供趋势图按分钟统计
func (h *watchHub) countRequest(failed bool) {
	m := now() / 60000
	h.mu.Lock()
	defer h.mu.Unlock()
	b := &h.buckets[m%watchMinutes]
	if b.minute != m {
		*b = watchBucket{minute: m}
	}
	b.reqs++
	if failed {
		b.errs++
	}
}

// series 返回最近 watchMinutes 分钟的请求数与失败数，最旧的在前
func (h *watchHub) series() (reqs, errs []int) {
	m := now() / 60000
	reqs, errs = make([]int, watchMinutes), make([]int, watchMinutes)
	h.mu.Lock()
	defer h.mu.Unlock()
	for i := range reqs {
		want := m - int64(watchMinutes-1-i)
		if b := h.buckets[want%watchMinutes]; b.minute == want {
			reqs[i], errs[i] = int(b.reqs), int(b.errs)
		}
	}
	return reqs, errs
}

// recent 返回最近 n 条事件，按时间从旧到新
func (h *watchHub) recent(n int) []watchEvent {
	h.mu.Lock()
	defer h.mu.Unlock()
	n = min(n, h.total, watchRing)
	out := make([]watchEvent, 0, n)
	for i := h.total - n; i < h.total; i++ {
		out = append(out, h.ring[i%watchRing])
	}
	return out
}

// subscribe 登记一个订阅者，同时返回此刻的回看事件。已关闭、连接数满、同一令牌连接过多都会拒绝
func (h *watchHub) subscribe(token string) (*watchSub, []watchEvent, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	select {
	case <-h.done:
		return nil, nil, fail("WATCH_CLOSED", "服务正在重启，请稍后重连", 503)
	default:
	}
	if len(h.subs) >= watchMaxStreams {
		return nil, nil, fail("WATCH_BUSY", "监控连接数已达上限，请先关闭不用的终端", 429)
	}
	if h.perTok[token] >= watchPerToken {
		return nil, nil, fail("WATCH_BUSY", "这个令牌已有太多监控连接，请先关闭不用的终端", 429)
	}
	s := &watchSub{ch: make(chan watchEvent, watchSubBuffer), token: token}
	h.subs[s] = struct{}{}
	h.perTok[token]++
	n := min(10, h.total, watchRing)
	back := make([]watchEvent, 0, n)
	for i := h.total - n; i < h.total; i++ {
		back = append(back, h.ring[i%watchRing])
	}
	return s, back, nil
}

func (h *watchHub) unsubscribe(s *watchSub) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.subs[s]; !ok {
		return
	}
	delete(h.subs, s)
	if h.perTok[s.token]--; h.perTok[s.token] <= 0 {
		delete(h.perTok, s.token)
	}
}

// takeDropped 取走并清零这个订阅者累计丢弃的事件数
func (h *watchHub) takeDropped(s *watchSub) int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := s.dropped
	s.dropped = 0
	return n
}

func (h *watchHub) watchers() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}

// closeAll 通知所有监控流结束，并拒绝新的订阅。可重复调用
func (h *watchHub) closeAll() { h.once.Do(func() { close(h.done) }) }
