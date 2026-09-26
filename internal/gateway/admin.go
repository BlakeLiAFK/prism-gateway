// 管理接口 action 分发与配置导入。
package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"prism-gateway/internal/sqlite"
	"runtime"
	"strings"
	"time"
)

func (a *App) call(ctx context.Context, action string, p Object) (any, error) {
	c := a.Store.Config()
	version := int64(num(p, "version"))
	id := str(p, "id")
	switch action {
	case "config.get":
		return c, nil
	case "config.export":
		return Object{
			"format":          "prism.config/1",
			"gateway_version": Version,
			"exported_at":     now(),
			"config":          c,
			"note":            "不含上游 API Key、客户端 Key 与管理员令牌；导入到新实例后需重新填写供应商凭证",
		}, nil
	case "config.import":
		return a.importConfig(version, p)
	case "system.info":
		return Object{"version": Version, "go": runtime.Version(), "sqlite": sqlite.Version(), "uptime_ms": now() - a.Started, "config_version": c.Version, "storage": "SQLite", "ui": "embed.FS", "management_api": "POST /api.json", "runtime": a.Engine.Health()}, nil
	case "dashboard.get":
		return a.dashboard(p)
	case "provider.list":
		return c.Providers, nil
	case "model.list":
		return c.Models, nil
	case "route.list":
		return c.Routes, nil
	case "alias.list":
		return c.Aliases, nil
	case "settings.get":
		return c.Settings, nil
	case "quota.list":
		return a.Engine.Quotas()
	case "provider.usage":
		return a.providerUsageAll(ctx), nil
	case "provider.save":
		v := Provider{ID: id, Enabled: true, Kind: "custom", Auth: "auto", TimeoutSec: 300}
		if id != "" {
			if old, ok := c.provider(id); ok {
				v = old
			}
		} else {
			v.ID = randomID("p_")
		}
		vals := obj(p["provider"])
		if vals == nil {
			return nil, fail("INVALID_PARAMS", "缺少 provider 对象", 400)
		}
		secret, changeSecret := vals["api_key"]
		adminSecret, changeAdmin := vals["admin_key"]
		valsCopy := Object{}
		for k, x := range vals {
			if k != "api_key" && k != "admin_key" {
				valsCopy[k] = x
			}
		}
		if er := decode(valsCopy, &v); er != nil {
			return nil, fail("INVALID_PARAMS", er.Error(), 400)
		}
		if changeSecret {
			v.Secret, _ = secret.(string)
		}
		if changeAdmin {
			v.AdminSecret, _ = adminSecret.(string)
		}
		v.HasKey = v.Secret != ""
		v.HasAdminKey = v.AdminSecret != ""
		return a.Store.Change(version, action, v.ID, func(c *Config) error {
			for i, x := range c.Providers {
				if x.ID == v.ID {
					c.Providers[i] = v
					return nil
				}
			}
			c.Providers = append(c.Providers, v)
			return nil
		})
	case "model.save":
		v := Model{ID: id, Enabled: true, Protocol: "chat", Tools: true, Context: 128000, MaxOutput: 4096, Concurrency: 2}
		if old, ok := c.model(id); ok {
			v = old
		}
		if er := decode(p["model"], &v); er != nil {
			return nil, fail("INVALID_PARAMS", er.Error(), 400)
		}
		if v.ID == "" {
			v.ID = randomID("m_")
		}
		return a.Store.Change(version, action, v.ID, func(c *Config) error {
			for i, x := range c.Models {
				if x.ID == v.ID {
					c.Models[i] = v
					return nil
				}
			}
			c.Models = append(c.Models, v)
			return nil
		})
	case "route.save":
		v := Route{ID: id, Enabled: true, Strategy: "priority", Affinity: true, Candidates: []Candidate{}}
		if old, ok := c.route(id); ok {
			v = old
		}
		if er := decode(p["route"], &v); er != nil {
			return nil, fail("INVALID_PARAMS", er.Error(), 400)
		}
		// 显式要求时顺带启用候选模型：加进路由就是要用它，与路由在同一事务里生效
		enable := boolean(p, "enable_models")
		return a.Store.Change(version, action, v.ID, func(c *Config) error {
			if enable {
				enableModels(c, v.Candidates)
			}
			for i, x := range c.Routes {
				if x.ID == v.ID {
					c.Routes[i] = v
					return nil
				}
			}
			c.Routes = append(c.Routes, v)
			return nil
		})
	case "route.reorder", "provider.reorder":
		// 顺序是一个整体，一次写完。逐个保存会在中途失败时留下一个
		// 比原来更错的顺序，而且每次保存都要递增配置版本。
		ids := arr(p["ids"])
		if len(ids) == 0 {
			return nil, fail("INVALID_PARAMS", "ids 不能为空", 400)
		}
		rank := map[string]int{}
		for i, v := range ids {
			rank[fmt.Sprint(v)] = (i + 1) * 10
		}
		return a.Store.Change(version, action, "", func(c *Config) error {
			applyOrder(c, action == "provider.reorder", rank)
			return nil
		})
	case "alias.save":
		v := Alias{Enabled: true}
		if er := decode(p["alias"], &v); er != nil {
			return nil, fail("INVALID_PARAMS", er.Error(), 400)
		}
		return a.Store.Change(version, action, v.ID, func(c *Config) error {
			for i, x := range c.Aliases {
				if x.ID == v.ID {
					c.Aliases[i] = v
					return nil
				}
			}
			c.Aliases = append(c.Aliases, v)
			return nil
		})
	case "settings.update":
		v := c.Settings
		if er := decode(p["settings"], &v); er != nil {
			return nil, fail("INVALID_PARAMS", er.Error(), 400)
		}
		// 监听地址改了就先把新地址占下来；占不到就直接失败，配置保持原样，
		// 服务继续跑在旧地址上，不会因为一次手滑把自己关在门外
		// 只有管理员真的改动了这一项才切换。基准是配置里的旧值，不是当前实际地址：
		// --listen 救援覆盖期间保存其它设置，不该把服务拽回数据库里那个坏地址。
		var pending net.Listener
		if a.Listen != nil && v.Listen != "" && !sameListen(v.Listen, c.Settings.Listen) && !a.Listen.SameAddr(v.Listen) {
			ln, er := a.Listen.Bind(v.Listen)
			if er != nil {
				return nil, er
			}
			pending = ln
		}
		cfg, er := a.Store.Change(version, action, "settings", func(c *Config) error { c.Settings = v; return nil })
		if er != nil {
			if pending != nil {
				pending.Close()
			}
			return nil, er
		}
		if pending != nil {
			a.Listen.Adopt(pending)
		}
		return cfg, nil
	case "provider.delete", "model.delete", "route.delete", "alias.delete":
		return a.Store.Change(version, action, id, func(c *Config) error {
			found := false
			switch action {
			case "provider.delete":
				xs := []Provider{}
				for _, v := range c.Providers {
					if v.ID == id {
						found = true
						continue
					}
					xs = append(xs, v)
				}
				c.Providers = xs
			case "model.delete":
				xs := []Model{}
				for _, v := range c.Models {
					if v.ID == id {
						found = true
						continue
					}
					xs = append(xs, v)
				}
				c.Models = xs
			case "route.delete":
				xs := []Route{}
				for _, v := range c.Routes {
					if v.ID == id {
						found = true
						continue
					}
					xs = append(xs, v)
				}
				c.Routes = xs
			case "alias.delete":
				xs := []Alias{}
				for _, v := range c.Aliases {
					if v.ID == id {
						found = true
						continue
					}
					xs = append(xs, v)
				}
				c.Aliases = xs
			}
			if !found {
				return fail("NOT_FOUND", "对象不存在", 404)
			}
			return nil
		})
	case "route.test":
		o := obj(p["request"])
		if o == nil {
			o = Object{"model": id, "messages": []any{Object{"role": "user", "content": "Describe this code."}}}
		}
		protocol := str(p, "protocol")
		if protocol == "" {
			protocol = "chat"
		}
		ss, reasons, er := a.Engine.selections(c, o, protocol, "")
		if er != nil {
			return nil, er
		}
		ranked := []any{}
		for _, s := range ss {
			ranked = append(ranked, Object{"model_id": s.Model.ID, "score": s.Score, "reason": s.Reason, "protocol": s.Model.Protocol})
		}
		note := "静态协议/配置模拟，不发起模型请求。实际调用仍须通过实时额度、冷却与并发检查。"
		if r, ok := c.route(id); ok && r.Strategy == "weighted" {
			note += "按权重分流每次抽签，排序会变化。"
		}
		return Object{"ranked": ranked, "checks": reasons, "note": note}, nil
	case "provider.test":
		pr, ok := c.provider(id)
		if !ok {
			return nil, fail("NOT_FOUND", "Provider 不存在", 404)
		}
		if pr.Kind == "mock" {
			return Object{"status": 200, "latency_ms": 0, "message": "本地演示 Provider 正常；未调用云端"}, nil
		}
		ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, "GET", strings.TrimRight(pr.BaseURL, "/")+"/models", nil)
		requestHeaders(req, &http.Request{Header: http.Header{}}, pr, providerProtocol(pr), "")
		t := now()
		res, er := a.Engine.client(pr).Do(req)
		if er != nil {
			return nil, fail("CONNECTION_FAILED", "上游连接失败；检查 URL、证书、私有网络权限与网络连通性", 502)
		}
		defer res.Body.Close()
		io.Copy(io.Discard, io.LimitReader(res.Body, 65536))
		return Object{"status": res.StatusCode, "latency_ms": now() - t, "message": "测试 GET /models；不是模型生成能力验证"}, nil
	case "provider.sync_models":
		return a.startSync(id)
	case "job.list":
		return a.Store.DB.Query("SELECT * FROM jobs ORDER BY created_at DESC LIMIT 100")
	case "job.get":
		r, er := a.Store.DB.Query("SELECT * FROM jobs WHERE id=?", id)
		if er != nil {
			return nil, er
		}
		if len(r) == 0 {
			return nil, fail("NOT_FOUND", "任务不存在", 404)
		}
		return r[0], nil
	case "apikey.list":
		return a.keyList(p)
	case "apikey.create":
		name := strings.TrimSpace(str(p, "name"))
		if name == "" || len(name) > 120 {
			return nil, fail("INVALID_PARAMS", "Key 名称不能为空且长度最多 120", 400)
		}
		allowed, er := keyTargets(c, p)
		if er != nil {
			return nil, er
		}
		k, er := keyPolicyFrom(p)
		if er != nil {
			return nil, er
		}
		key := randomID("prism_sk_")
		kid := randomID("key_")
		er = a.Store.DB.Exec("INSERT INTO api_keys(id,name,prefix,digest,enabled,allowed,created_at,"+keyPolicyColumns+") VALUES (?,?,?,?,1,?,?,?,?,?,?)", kid, name, key[:17], digest(key), raw(allowed), now(), k.ExpiresAt, k.LimitDay, k.LimitMonth, k.RPM)
		if er != nil {
			return nil, er
		}
		a.audit(action, kid)
		return Object{"id": kid, "key": key, "warning": "完整密钥只显示这一次；请保存在密码管理器中。"}, nil
	case "apikey.revoke":
		er := a.Store.DB.Exec("UPDATE api_keys SET enabled=0,revoked_at=? WHERE id=?", now(), id)
		a.Engine.forgetKeys()
		if er == nil {
			a.audit(action, id)
		}
		return Object{"id": id, "revoked": true}, er
	case "session.list":
		ttl := int64(c.Settings.SessionTTLHours) * 3600000
		return a.Store.DB.Query("SELECT *, updated_at+? expires_at FROM sessions WHERE updated_at>? ORDER BY updated_at DESC LIMIT 200", ttl, now()-ttl)
	case "session.delete":
		return a.deleteSessions(id, str(p, "model_id"))
	case "request.list":
		return a.requests(p)
	case "request.get":
		r, er := a.Store.DB.Query("SELECT * FROM requests WHERE id=?", id)
		if er != nil {
			return nil, er
		}
		if len(r) == 0 {
			return nil, fail("NOT_FOUND", "请求不存在", 404)
		}
		return r[0], nil
	case "audit.list":
		return a.Store.DB.Query("SELECT * FROM audit_logs ORDER BY created_at DESC LIMIT 200")
	case "playground.run":
		protocol := str(p, "protocol")
		if protocol != "chat" && protocol != "messages" && protocol != "responses" && protocol != "systemone" {
			return nil, fail("INVALID_PARAMS", "protocol 无效", 400)
		}
		body := Object{"model": str(p, "model")}
		if protocol == "systemone" {
			// System One 的载荷是状态加问题，没有 prompt 也没有流式开关
			st := str(p, "state")
			if len(st) > 64000 || st == "" {
				return nil, fail("INVALID_PARAMS", "请输入不超过 64KB 的状态内容", 400)
			}
			body["state"] = st
			body["questions"] = obj(p["questions"])
			if er := validateSystemOne(body); er != nil {
				return nil, fail("INVALID_PARAMS", er.Error(), 400)
			}
		} else {
			prompt := str(p, "prompt")
			if len(prompt) > 64000 || prompt == "" {
				return nil, fail("INVALID_PARAMS", "请输入不超过 64KB 的提示词", 400)
			}
			body["stream"] = false
			if protocol == "responses" {
				body["input"] = prompt
				body["max_output_tokens"] = 512
			} else {
				body["messages"] = []any{Object{"role": "user", "content": prompt}}
				body["max_tokens"] = 512
			}
		}
		req, _ := http.NewRequestWithContext(ctx, "POST", "http://internal"+pathFor(protocol), bytes.NewBufferString(raw(body)))
		req.Header.Set("X-Prism-Session", str(p, "session"))
		rr := httptest.NewRecorder()
		t := now()
		a.Engine.Handle(rr, req, protocol, Principal{ID: "admin-playground"})
		var result any
		json.Unmarshal(rr.Body.Bytes(), &result)
		return Object{"status": rr.Code, "duration_ms": now() - t, "headers": rr.Header(), "response": result}, nil
	case "backup.create":
		path, size, er := a.Store.Backup("")
		if er != nil {
			return nil, er
		}
		a.audit("backup.create", filepath.Base(path))
		slog.Info("配置备份已生成", "path", path, "bytes", size)
		return Object{"path": path, "bytes": size,
			"note": "快照不含 .key 主密钥；恢复上游凭证必须配套原主密钥"}, nil
	case "backup.list":
		return a.Store.Backups()
	case "demo.enable":
		return a.EnableDemo(version)
	case "batch.read":
		requests := arr(p["requests"])
		if len(requests) < 1 || len(requests) > 10 {
			return nil, fail("INVALID_PARAMS", "batch.read 允许 1–10 个只读请求", 400)
		}
		out := []any{}
		for _, v := range requests {
			v := obj(v)
			act := str(v, "action")
			switch act {
			case "config.get", "config.export", "backup.list", "system.info", "dashboard.get", "quota.list", "provider.usage", "provider.list", "model.list", "route.list", "request.list", "session.list", "job.list", "apikey.list", "route.stats", "model.stats", "model.usage", "key.usage":
				data, er := a.call(ctx, act, obj(v["params"]))
				if er != nil {
					_, code, msg := errorParts(er)
					out = append(out, Object{"ok": false, "error": Object{"code": code, "message": msg}})
				} else {
					out = append(out, Object{"ok": true, "data": data})
				}
			default:
				return nil, fail("ACTION_NOT_ALLOWED", "batch.read 中的 action 不受支持", 400)
			}
		}
		return out, nil
	default:
		if f, ok := consoleActions[action]; ok {
			return f(a, id, p)
		}
		return nil, fail("UNKNOWN_ACTION", "未知管理 action: "+action, 400)
	}
}

// importConfig 用导出的快照整体替换业务配置。
// 导出文件里没有凭证，所以按 provider id 保留库中已有的密钥；
// 文件带来的新供应商没有密钥，返回值会列出需要补填的 id。
// 客户端 Key、管理员令牌、请求记录和用量都不在替换范围内。
func (a *App) importConfig(version int64, p Object) (any, error) {
	src := obj(p["config"])
	if src == nil {
		return nil, fail("INVALID_PARAMS", "缺少 config 对象；请提交 config.export 的 config 字段", 400)
	}
	if f := str(p, "format"); f != "" && f != "prism.config/1" {
		return nil, fail("UNSUPPORTED_FORMAT", "不认识的导出格式："+f, 400)
	}
	var in Config
	dec := json.NewDecoder(bytes.NewReader([]byte(raw(src))))
	dec.DisallowUnknownFields()
	if e := dec.Decode(&in); e != nil {
		return nil, fail("INVALID_CONFIG", "配置结构无法解析："+e.Error(), 400)
	}
	c, err := a.Store.Change(version, "config.import", "config", func(cur *Config) error {
		kept := map[string]Provider{}
		for _, old := range cur.Providers {
			kept[old.ID] = old
		}
		cur.Providers = append([]Provider{}, in.Providers...)
		for i := range cur.Providers {
			p := &cur.Providers[i]
			p.Secret, p.AdminSecret = kept[p.ID].Secret, kept[p.ID].AdminSecret
			// has_key 由库中实际凭证决定，不接受导入文件的声明
			p.HasKey, p.HasAdminKey = p.Secret != "", p.AdminSecret != ""
		}
		cur.Models = append([]Model{}, in.Models...)
		cur.Routes = append([]Route{}, in.Routes...)
		cur.Aliases = append([]Alias{}, in.Aliases...)
		cur.Settings = in.Settings
		return nil
	})
	if err != nil {
		return nil, err
	}
	missing := []any{}
	for _, v := range c.Providers {
		if !v.HasKey && v.Auth != "none" {
			missing = append(missing, v.ID)
		}
	}
	return Object{"version": c.Version, "providers": len(c.Providers), "models": len(c.Models),
		"routes": len(c.Routes), "aliases": len(c.Aliases), "providers_missing_key": missing}, nil
}
