package gateway

// 终端监控令牌：只能打开 watch.stream，不能调用任何其他管理 action。
// 与管理员令牌、网关 Key、上游 Key 都是独立的凭证。命令会留在终端历史里，
// 所以这个令牌只读、有有效期、可随时撤销，库里只存摘要。

import (
	"log/slog"
	"strings"
)

const watchTokenTable = `CREATE TABLE IF NOT EXISTS watch_tokens (id TEXT PRIMARY KEY, name TEXT NOT NULL, digest TEXT UNIQUE NOT NULL,
	created_at INTEGER NOT NULL, expires_at INTEGER NOT NULL, last_used_at INTEGER NOT NULL DEFAULT 0, revoked_at INTEGER NOT NULL DEFAULT 0)`

const (
	watchTokenPrefix      = "prism_watch_"
	watchTokenMax         = 20 // 同时有效的令牌数上限，防止遗忘的令牌越积越多
	watchTokenDefaultDays = 7
	watchTokenMaxDays     = 30
)

func init() {
	consoleActions["watch.token_create"] = func(a *App, _ string, p Object) (any, error) { return a.createWatchToken(p) }
	consoleActions["watch.token_list"] = func(a *App, _ string, _ Object) (any, error) { return a.listWatchTokens() }
	consoleActions["watch.token_revoke"] = func(a *App, id string, _ Object) (any, error) { return a.revokeWatchToken(id) }
}

func (a *App) createWatchToken(p Object) (any, error) {
	name := strings.TrimSpace(str(p, "name"))
	if name == "" {
		name = "terminal"
	}
	if len([]rune(name)) > 80 {
		return nil, fail("INVALID_PARAMS", "令牌名称最长 80 个字符", 400)
	}
	days := int(num(p, "days"))
	if days == 0 {
		days = watchTokenDefaultDays
	}
	if days < 1 || days > watchTokenMaxDays {
		return nil, fail("INVALID_PARAMS", "有效期为 1 到 30 天", 400)
	}
	n, err := a.Store.DB.Query("SELECT COUNT(*) n FROM watch_tokens WHERE revoked_at=0 AND expires_at>?", now())
	if err != nil {
		return nil, err
	}
	if n[0].Int("n") >= watchTokenMax {
		return nil, fail("TOKEN_LIMIT", "有效的监控令牌已达上限，请先撤销不用的", 400)
	}
	token, id, expires := randomID(watchTokenPrefix), randomID("wt_"), now()+int64(days)*86400000
	if err := a.Store.DB.Exec("INSERT INTO watch_tokens(id,name,digest,created_at,expires_at) VALUES (?,?,?,?,?)", id, name, digest(token), now(), expires); err != nil {
		return nil, err
	}
	a.audit("watch.token_create", id)
	return Object{"id": id, "token": token, "name": name, "expires_at": expires,
		"warning": "完整令牌只显示这一次；它只能打开终端监控流，不能修改任何配置。"}, nil
}

func (a *App) listWatchTokens() (any, error) {
	return a.Store.DB.Query("SELECT id,name,created_at,expires_at,last_used_at FROM watch_tokens WHERE revoked_at=0 AND expires_at>? ORDER BY created_at DESC", now())
}

func (a *App) revokeWatchToken(id string) (any, error) {
	rows, err := a.Store.DB.Query("SELECT id FROM watch_tokens WHERE id=? AND revoked_at=0", id)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, fail("NOT_FOUND", "监控令牌不存在或已撤销", 404)
	}
	if err := a.Store.DB.Exec("UPDATE watch_tokens SET revoked_at=? WHERE id=?", now(), id); err != nil {
		return nil, err
	}
	a.audit("watch.token_revoke", id)
	return Object{"id": id}, nil
}

// checkWatchToken 校验令牌并返回它的 ID 与到期时刻。只认监控令牌，管理员令牌与网关 Key 一律不认
func (a *App) checkWatchToken(token string) (string, int64, bool) {
	if !strings.HasPrefix(token, watchTokenPrefix) || len(token) < 40 {
		return "", 0, false
	}
	rows, err := a.Store.DB.Query("SELECT id,expires_at FROM watch_tokens WHERE digest=? AND revoked_at=0 AND expires_at>?", digest(token), now())
	if err != nil || len(rows) != 1 {
		return "", 0, false
	}
	return rows[0].String("id"), rows[0].Int("expires_at"), true
}

// touchWatchToken 记录最近一次使用，后台列表据此判断哪些令牌可以清掉
func (a *App) touchWatchToken(id string) {
	if err := a.Store.DB.Exec("UPDATE watch_tokens SET last_used_at=? WHERE id=?", now(), id); err != nil {
		slog.Error("watch token touch failed", "err", err)
	}
}
