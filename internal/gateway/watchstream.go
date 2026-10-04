package gateway

// watch.stream：把监控事件以纯文本持续推给 curl。
// 走现有的 POST /api.json，不新增 URL；响应是分块传输的 text/plain，而不是带 data: 前缀的标准 SSE，
// 这样 curl 直接打印出来才干净。认证只认监控令牌：它只能开这个流，调用其他 action 一律当作未登录。

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// 这些间隔是变量，测试里会缩短
var (
	watchTick    = 2 * time.Second  // 仪表盘刷新间隔
	watchRecheck = 5 * time.Second  // 复核令牌是否已撤销或过期
	watchQuiet   = 20 * time.Second // 日志模式连续这么久没有事件就输出一行状态，也顺带保活
)

type watchOpts struct {
	dashboard     bool
	width, height int
	color         bool
}

func parseWatchOpts(p Object) (watchOpts, error) {
	o := watchOpts{dashboard: true, width: 100, height: 40, color: boolean(p, "color")}
	switch str(p, "mode") {
	case "", "dashboard":
	case "log":
		o.dashboard, o.width = false, 240
	default:
		return o, fail("INVALID_PARAMS", "mode 只能是 dashboard 或 log", 400)
	}
	if p["width"] != nil {
		o.width = clamp(int(num(p, "width")), 60, 240)
	}
	if p["height"] != nil {
		o.height = clamp(int(num(p, "height")), 12, 80)
	}
	return o, nil
}

// watchOut 负责写出并立刻推送。每次写之前都续一个写超时：终端卡住不读时，这条流在十秒内放弃
type watchOut struct {
	w  io.Writer
	rc *http.ResponseController
}

func (o *watchOut) write(s string) error {
	if err := o.rc.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return err
	}
	if _, err := io.WriteString(o.w, s); err != nil {
		return err
	}
	if err := o.rc.Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return err
	}
	return nil
}

func (a *App) watchStream(w http.ResponseWriter, r *http.Request, p Object, send func(any, error)) {
	token := bearer(r)
	tid, expires, ok := a.checkWatchToken(token)
	if !ok {
		// 键里带上真实客户端地址（可信代理时取转发地址）：代理后面所有请求的直连地址都一样，
		// 按直连地址计数会让任何人乱发令牌就把所有人锁在外面，也会占用管理员登录的额度
		if !a.attemptAllowed("watch:" + clientOf(r).IP) {
			send(nil, fail("LOGIN_RATE_LIMIT", "尝试过于频繁，请稍后再试", 429))
			return
		}
		send(nil, fail("UNAUTHORIZED", "需要有效的监控令牌：在后台「系统设置 / 终端监控」生成；管理员令牌与网关 Key 不能用于监控流", 401))
		return
	}
	opts, err := parseWatchOpts(p)
	if err != nil {
		send(nil, err)
		return
	}
	hub := a.Engine.watch
	sub, backlog, err := hub.subscribe(tid)
	if err != nil {
		send(nil, err)
		return
	}
	defer hub.unsubscribe(sub)
	a.touchWatchToken(tid)
	zone := time.UTC
	if sc, err := a.loadSchedule(); err == nil {
		zone = sc.zone()
	}
	h := w.Header()
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	s := &watchSession{a: a, hub: hub, sub: sub, out: &watchOut{w: w, rc: http.NewResponseController(w)}, opts: opts, zone: zone, token: token, expires: expires}
	s.run(r.Context(), backlog)
}

type watchSession struct {
	a       *App
	hub     *watchHub
	sub     *watchSub
	out     *watchOut
	opts    watchOpts
	zone    *time.Location
	token   string
	expires int64
	lastOut time.Time
}

func (s *watchSession) sys(text string) error {
	return s.out.write(eventLine(watchEvent{At: now(), Kind: "SYS", Text: text}, s.zone, s.opts.width, s.opts.color) + "\n")
}

// run 是推流主循环：事件、定时刷新、令牌复核、服务停止、客户端断开，哪个先到处理哪个
func (s *watchSession) run(ctx context.Context, backlog []watchEvent) {
	tickEvery := watchTick
	if !s.opts.dashboard {
		tickEvery = max(watchQuiet/4, 10*time.Millisecond)
		left := time.Duration(s.expires-now()) * time.Millisecond
		if s.sys(fmt.Sprintf("prism gateway %s  log stream  token expires in %s  Ctrl+C to quit", Version, fmtUptime(left))) != nil {
			return
		}
		for _, ev := range backlog {
			if s.out.write(eventLine(ev, s.zone, s.opts.width, s.opts.color)+"\n") != nil {
				return
			}
		}
	} else if s.out.write("\x1b[2J") != nil {
		return
	}
	s.lastOut = time.Now()
	if s.opts.dashboard && s.onTick() != nil {
		return
	}
	tick, recheck := time.NewTicker(tickEvery), time.NewTicker(watchRecheck)
	defer tick.Stop()
	defer recheck.Stop()
	for {
		var err error
		select {
		case <-ctx.Done():
			return
		case <-s.hub.done:
			s.sys("gateway is restarting, reconnecting soon")
			return
		case <-recheck.C:
			if _, _, ok := s.a.checkWatchToken(s.token); !ok {
				s.sys("token revoked or expired, stopping")
				return
			}
		case ev := <-s.sub.ch:
			err = s.onEvent(ev)
		case <-tick.C:
			err = s.onTick()
		}
		if err != nil {
			return
		}
	}
}

// onEvent：日志模式逐条写出；仪表盘从回看缓冲取事件，这里只需把通道读空
func (s *watchSession) onEvent(ev watchEvent) error {
	if s.opts.dashboard {
		return nil
	}
	s.lastOut = time.Now()
	if err := s.out.write(eventLine(ev, s.zone, s.opts.width, s.opts.color) + "\n"); err != nil {
		return err
	}
	if n := s.hub.takeDropped(s.sub); n > 0 {
		return s.sys(fmt.Sprintf("%d events dropped, terminal too slow", n))
	}
	return nil
}

func (s *watchSession) onTick() error {
	if !s.opts.dashboard {
		if time.Since(s.lastOut) < watchQuiet {
			return nil
		}
		s.lastOut = time.Now()
		return s.sys(fmt.Sprintf("alive, no new events in %s  active %d  watchers %d", fmtUptime(watchQuiet), s.a.Engine.activeNow(), s.hub.watchers()))
	}
	v, err := s.a.dashView(s.opts, s.zone, s.expires)
	if err != nil {
		return s.sys("snapshot failed: " + err.Error())
	}
	var b strings.Builder
	b.WriteString("\x1b[H")
	for _, l := range renderDashboard(v) {
		b.WriteString(l + "\x1b[K\n")
	}
	b.WriteString("\x1b[J")
	return s.out.write(b.String())
}
