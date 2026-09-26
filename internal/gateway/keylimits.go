package gateway

// 网关 Key 的到期时间与限额：近 24 小时 / 近 30 天花费上限、每分钟请求数上限。
// 花费取自 usage_hourly，只含已结束的请求并缓存 10 秒，所以可能略超上限；
// 这是给共享出去的 Key 设的护栏，不是计费级的精确预算。

import (
	"strings"
	"time"
)

type keyPolicy struct {
	ExpiresAt  int64   `json:"expires_at"`
	LimitDay   float64 `json:"limit_day"`
	LimitMonth float64 `json:"limit_month"`
	RPM        int     `json:"rpm"`
}

const keyPolicyColumns = "expires_at,limit_day,limit_month,rpm"

func init() {
	consoleActions["apikey.update"] = func(a *App, id string, p Object) (any, error) { return a.updateKey(id, p) }
}

func keyPolicyFrom(p Object) (keyPolicy, error) {
	k := keyPolicy{ExpiresAt: int64(num(p, "expires_at")), LimitDay: num(p, "limit_day"), LimitMonth: num(p, "limit_month"), RPM: int(num(p, "rpm"))}
	if k.ExpiresAt < 0 || k.LimitDay < 0 || k.LimitMonth < 0 || k.RPM < 0 || k.RPM > 100000 {
		return k, fail("INVALID_PARAMS", "到期时间与限额不能为负数，RPM 最多 100000", 400)
	}
	return k, nil
}

// keyTargets 校验 Key 可请求的模型 / 路由 / 别名
func keyTargets(c Config, p Object) ([]string, error) {
	allowed := []string{}
	for _, v := range arr(p["allowed"]) {
		s, ok := v.(string)
		if !ok {
			return nil, fail("INVALID_PARAMS", "allowed 应为字符串数组", 400)
		}
		_, isModel := c.model(s)
		_, isRoute := c.route(s)
		isAlias := false
		for _, x := range c.Aliases {
			isAlias = isAlias || x.ID == s
		}
		if !isModel && !isRoute && !isAlias {
			return nil, fail("INVALID_PARAMS", "授权目标不存在: "+s, 400)
		}
		allowed = append(allowed, s)
	}
	return allowed, nil
}

// updateKey 修改名称、授权范围、到期时间与限额；密钥本身不变
func (a *App) updateKey(id string, p Object) (any, error) {
	name := strings.TrimSpace(str(p, "name"))
	if id == "" || name == "" || len(name) > 120 {
		return nil, fail("INVALID_PARAMS", "需要 Key id，名称不能为空且长度最多 120", 400)
	}
	allowed, err := keyTargets(a.Store.Config(), p)
	if err != nil {
		return nil, err
	}
	k, err := keyPolicyFrom(p)
	if err != nil {
		return nil, err
	}
	if err = a.Store.DB.Exec("UPDATE api_keys SET name=?,allowed=?,expires_at=?,limit_day=?,limit_month=?,rpm=? WHERE id=?",
		name, raw(allowed), k.ExpiresAt, k.LimitDay, k.LimitMonth, k.RPM, id); err != nil {
		return nil, err
	}
	a.Engine.forgetKeys()
	a.audit("apikey.update", id)
	return Object{"id": id}, nil
}

type keySpendEntry struct {
	at         int64
	day, month float64
}

// keySpend 返回 Key 近 24 小时与近 30 天的估算花费，缓存 10 秒
func (e *Engine) keySpend(id string) (day, month float64, err error) {
	e.keyMu.Lock()
	c, ok := e.spend[id]
	e.keyMu.Unlock()
	if ok && now()-c.at < 10000 {
		return c.day, c.month, nil
	}
	rows, err := e.store.DB.Query(`SELECT COALESCE(SUM(CASE WHEN hour>=? THEN cost_nano END),0) d, COALESCE(SUM(cost_nano),0) m
		FROM usage_hourly WHERE key_id=? AND hour>=?`, sinceHour(1), id, sinceHour(30))
	if err != nil {
		return 0, 0, err
	}
	day, month = dollars(rows[0].Int("d")), dollars(rows[0].Int("m"))
	e.keyMu.Lock()
	e.spend[id] = keySpendEntry{now(), day, month}
	e.keyMu.Unlock()
	return day, month, nil
}

// checkKey 在生成请求开始前检查 Key 的 RPM 与花费上限
func (e *Engine) checkKey(p Principal) error {
	k := p.Policy
	if k.RPM > 0 {
		e.keyMu.Lock()
		t := now()
		hits := e.keyHits[p.ID][:0]
		for _, ts := range e.keyHits[p.ID] {
			if ts > t-60000 {
				hits = append(hits, ts)
			}
		}
		full := len(hits) >= k.RPM
		if !full {
			hits = append(hits, t)
		}
		e.keyHits[p.ID] = hits
		e.keyMu.Unlock()
		if full {
			return fail("KEY_RPM_LIMIT", "此网关 Key 的每分钟请求数已达上限", 429)
		}
	}
	if k.LimitDay > 0 || k.LimitMonth > 0 {
		day, month, err := e.keySpend(p.ID)
		if err != nil {
			return err
		}
		if k.LimitDay > 0 && day >= k.LimitDay {
			return fail("KEY_BUDGET_LIMIT", "此网关 Key 近 24 小时的估算花费已达上限", 429)
		}
		if k.LimitMonth > 0 && month >= k.LimitMonth {
			return fail("KEY_BUDGET_LIMIT", "此网关 Key 近 30 天的估算花费已达上限", 429)
		}
	}
	return nil
}

// expired 判断 Key 是否已过到期时间
func (k keyPolicy) expired() bool { return k.ExpiresAt > 0 && time.Now().UnixMilli() >= k.ExpiresAt }
