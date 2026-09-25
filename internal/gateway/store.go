package gateway

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"prism-gateway/internal/sqlite"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Secrets and SQLite configuration intentionally have different trust boundaries.
type Store struct {
	DB       *sqlite.DB
	aead     cipher.AEAD
	mu       sync.Mutex
	snapshot atomic.Pointer[Config]
	KeyPath  string
}

func randomID(prefix string) string {
	b := make([]byte, 24)
	if _, e := rand.Read(b); e != nil {
		panic(e)
	}
	return prefix + base64.RawURLEncoding.EncodeToString(b)
}
func digest(s string) string { b := sha256.Sum256([]byte(s)); return hex.EncodeToString(b[:]) }
func OpenStore(path string) (*Store, error) {
	dir := filepath.Dir(path)
	if e := os.MkdirAll(dir, 0700); e != nil {
		return nil, e
	}
	kp := path + ".key"
	key, e := os.ReadFile(kp)
	if os.IsNotExist(e) {
		key = make([]byte, 32)
		if _, e = rand.Read(key); e != nil {
			return nil, e
		}
		f, e := os.OpenFile(kp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if e != nil {
			return nil, e
		}
		_, e = f.Write(key)
		if e == nil {
			e = f.Sync()
		}
		f.Close()
		if e != nil {
			return nil, e
		}
	} else if e != nil {
		return nil, e
	}
	if len(key) != 32 {
		return nil, errors.New("数据库加密主密钥必须为 32 字节，请勿删除已有 .key 文件")
	}
	b, e := aes.NewCipher(key)
	if e != nil {
		return nil, e
	}
	a, e := cipher.NewGCM(b)
	if e != nil {
		return nil, e
	}
	d, e := sqlite.Open(path)
	if e != nil {
		return nil, e
	}
	os.Chmod(path, 0600)
	s := &Store{DB: d, aead: a, KeyPath: kp}
	if e = s.migrate(); e != nil {
		d.Close()
		return nil, e
	}
	if e = s.load(); e != nil {
		d.Close()
		return nil, e
	}
	return s, nil
}
func (s *Store) encrypt(text string) string {
	if text == "" {
		return ""
	}
	nonce := make([]byte, s.aead.NonceSize())
	if _, e := rand.Read(nonce); e != nil {
		panic(e)
	}
	v := s.aead.Seal(nonce, nonce, []byte(text), []byte("prism-provider-v1"))
	return base64.StdEncoding.EncodeToString(v)
}
func (s *Store) decrypt(text string) (string, error) {
	if text == "" {
		return "", nil
	}
	v, e := base64.StdEncoding.DecodeString(text)
	if e != nil || len(v) < s.aead.NonceSize() {
		return "", errors.New("invalid encrypted credential")
	}
	n := s.aead.NonceSize()
	out, e := s.aead.Open(nil, v[:n], v[n:], []byte("prism-provider-v1"))
	return string(out), e
}
func (s *Store) migrate() error {
	return s.DB.Transaction(func(t *sqlite.Tx) error {
		qs := []string{
			`CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
			`CREATE TABLE IF NOT EXISTS providers (id TEXT PRIMARY KEY, body TEXT NOT NULL, secret TEXT NOT NULL DEFAULT '')`,
			`CREATE TABLE IF NOT EXISTS models (id TEXT PRIMARY KEY, provider_id TEXT NOT NULL REFERENCES providers(id), body TEXT NOT NULL)`,
			`CREATE TABLE IF NOT EXISTS routes (id TEXT PRIMARY KEY, body TEXT NOT NULL)`,
			`CREATE TABLE IF NOT EXISTS route_models (route_id TEXT REFERENCES routes(id), model_id TEXT REFERENCES models(id), position INTEGER NOT NULL, weight INTEGER NOT NULL, PRIMARY KEY(route_id,model_id))`,
			`CREATE TABLE IF NOT EXISTS aliases (id TEXT PRIMARY KEY, body TEXT NOT NULL)`,
			`CREATE TABLE IF NOT EXISTS settings (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
			`CREATE TABLE IF NOT EXISTS api_keys (id TEXT PRIMARY KEY, name TEXT NOT NULL, prefix TEXT NOT NULL, digest TEXT UNIQUE NOT NULL, enabled INTEGER NOT NULL, allowed TEXT NOT NULL, created_at INTEGER NOT NULL, last_used INTEGER, revoked_at INTEGER)`,
			`CREATE TABLE IF NOT EXISTS admin_sessions (digest TEXT PRIMARY KEY, csrf TEXT NOT NULL, expires_at INTEGER NOT NULL)`,
			`CREATE TABLE IF NOT EXISTS sessions (id TEXT PRIMARY KEY, key_id TEXT NOT NULL, model_id TEXT NOT NULL, provider_id TEXT NOT NULL, updated_at INTEGER NOT NULL, requests INTEGER NOT NULL DEFAULT 1)`,
			`CREATE TABLE IF NOT EXISTS requests (id TEXT PRIMARY KEY, parent_id TEXT NOT NULL, key_id TEXT NOT NULL, requested_model TEXT NOT NULL, model_id TEXT NOT NULL, provider_id TEXT NOT NULL, protocol TEXT NOT NULL, upstream_protocol TEXT NOT NULL, session_id TEXT NOT NULL, status TEXT NOT NULL, http_status INTEGER NOT NULL DEFAULT 0, started_at INTEGER NOT NULL, duration_ms INTEGER NOT NULL DEFAULT 0, input_tokens INTEGER NOT NULL DEFAULT 0, output_tokens INTEGER NOT NULL DEFAULT 0, cache_tokens INTEGER NOT NULL DEFAULT 0, write_tokens INTEGER NOT NULL DEFAULT 0, cost_nano INTEGER NOT NULL DEFAULT 0, cost_known INTEGER NOT NULL DEFAULT 0, usage_mode TEXT NOT NULL DEFAULT 'reserved', reason TEXT NOT NULL, error_code TEXT NOT NULL DEFAULT '', is_demo INTEGER NOT NULL DEFAULT 0, client_ip TEXT NOT NULL DEFAULT '', user_agent TEXT NOT NULL DEFAULT '', agent_role TEXT NOT NULL DEFAULT '')`,
			`CREATE INDEX IF NOT EXISTS requests_model_time ON requests(model_id,started_at)`,
			`CREATE INDEX IF NOT EXISTS requests_time ON requests(started_at)`,
			`CREATE TABLE IF NOT EXISTS jobs (id TEXT PRIMARY KEY, action TEXT NOT NULL, status TEXT NOT NULL, result TEXT, error TEXT, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL)`,
			`CREATE TABLE IF NOT EXISTS audit_logs (id TEXT PRIMARY KEY, action TEXT NOT NULL, target TEXT NOT NULL, version INTEGER NOT NULL, created_at INTEGER NOT NULL)`,
			`INSERT OR IGNORE INTO meta VALUES ('schema_version','1')`,
			`INSERT OR IGNORE INTO meta VALUES ('config_version','1')`,
			`UPDATE jobs SET status='interrupted', error='进程重启中断任务，请手动重试' WHERE status IN ('queued','running')`,
			`UPDATE requests SET status='unknown', usage_mode='reserved_after_restart', error_code='PROCESS_INTERRUPTED' WHERE status='running'`,
		}
		for _, q := range qs {
			if e := t.Exec(q); e != nil {
				return e
			}
		}
		// 老库补列。SQLite 没有 ADD COLUMN IF NOT EXISTS，先照着表结构看一眼有没有
		cols, e := t.Query("PRAGMA table_info(requests)")
		if e != nil {
			return e
		}
		have := map[string]bool{}
		for _, c := range cols {
			have[c.String("name")] = true
		}
		for _, c := range []string{"client_ip", "user_agent", "agent_role"} {
			if !have[c] {
				if e := t.Exec("ALTER TABLE requests ADD COLUMN " + c + " TEXT NOT NULL DEFAULT ''"); e != nil {
					return e
				}
			}
		}
		if cols, e = t.Query("PRAGMA table_info(providers)"); e != nil {
			return e
		}
		if !slices.ContainsFunc(cols, func(c sqlite.Row) bool { return c.String("name") == "admin_secret" }) {
			if e := t.Exec("ALTER TABLE providers ADD COLUMN admin_secret TEXT NOT NULL DEFAULT ''"); e != nil {
				return e
			}
		}
		r, e := t.Query("SELECT value FROM meta WHERE key='schema_version'")
		if e != nil {
			return e
		}
		if len(r) != 1 || r[0].String("value") != "1" {
			return errors.New("数据库 schema 版本不受此二进制支持")
		}
		return nil
	})
}
func (s *Store) load() error {
	c := defaults()
	r, e := s.DB.Query("SELECT CAST(value AS INTEGER) v FROM meta WHERE key='config_version'")
	if e != nil {
		return e
	}
	c.Version = r[0].Int("v")
	r, e = s.DB.Query("SELECT body,secret,admin_secret FROM providers ORDER BY id")
	if e != nil {
		return e
	}
	for _, row := range r {
		var p Provider
		if e = json.Unmarshal([]byte(row.String("body")), &p); e != nil {
			return e
		}
		p.Secret, e = s.decrypt(row.String("secret"))
		if e != nil {
			return fmt.Errorf("无法解密 Provider 凭证，请恢复与数据库配套的 .key 文件: %w", e)
		}
		p.HasKey = p.Secret != ""
		if p.AdminSecret, e = s.decrypt(row.String("admin_secret")); e != nil {
			return fmt.Errorf("无法解密 Provider 凭证，请恢复与数据库配套的 .key 文件: %w", e)
		}
		p.HasAdminKey = p.AdminSecret != ""
		c.Providers = append(c.Providers, p)
	}
	for _, table := range []string{"models", "routes", "aliases"} {
		r, e = s.DB.Query("SELECT body FROM " + table + " ORDER BY id")
		if e != nil {
			return e
		}
		for _, row := range r {
			switch table {
			case "models":
				var v Model
				e = json.Unmarshal([]byte(row.String("body")), &v)
				c.Models = append(c.Models, v)
			case "routes":
				var v Route
				e = json.Unmarshal([]byte(row.String("body")), &v)
				c.Routes = append(c.Routes, v)
			case "aliases":
				var v Alias
				e = json.Unmarshal([]byte(row.String("body")), &v)
				c.Aliases = append(c.Aliases, v)
			}
			if e != nil {
				return e
			}
		}
	}
	sortConfig(&c)
	r, e = s.DB.Query("SELECT key,value FROM settings")
	if e != nil {
		return e
	}
	m := Object{}
	json.Unmarshal([]byte(raw(c.Settings)), &m)
	for _, row := range r {
		var v any
		if e = json.Unmarshal([]byte(row.String("value")), &v); e != nil {
			return e
		}
		m[row.String("key")] = v
	}
	if e = decode(m, &c.Settings); e != nil {
		return e
	}
	s.snapshot.Store(&c)
	applyLogSettings(c.Settings)
	return nil
}

// Backup 用 SQLite 的 VACUUM INTO 生成一致性快照，不需要停进程，
// 也不会漏掉 WAL 里尚未合并的内容。
// 快照里的上游凭证仍是密文：恢复时必须配套原来的 .key，否则无法解密。
func (s *Store) Backup() (string, int64, error) {
	dbPath := strings.TrimSuffix(s.KeyPath, ".key")
	dir := filepath.Join(filepath.Dir(dbPath), "backups")
	if e := os.MkdirAll(dir, 0700); e != nil {
		return "", 0, e
	}
	// 带毫秒，避免同一秒内连续备份撞名
	stamp := strings.Replace(time.Now().Format("20060102-150405.000"), ".", "-", 1)
	target := filepath.Join(dir, "gateway-"+stamp+".db")
	if _, e := os.Stat(target); e == nil {
		return "", 0, fail("BACKUP_EXISTS", "同名备份已存在，请稍后重试", 409)
	}
	// VACUUM INTO 拒绝写入已存在的文件，不会静默覆盖既有备份
	if e := s.DB.Exec("VACUUM INTO ?", target); e != nil {
		return "", 0, e
	}
	os.Chmod(target, 0600)
	fi, e := os.Stat(target)
	if e != nil {
		return "", 0, e
	}
	return target, fi.Size(), nil
}

// Backups 列出已有快照，按时间倒序。
func (s *Store) Backups() ([]any, error) {
	dir := filepath.Join(filepath.Dir(strings.TrimSuffix(s.KeyPath, ".key")), "backups")
	entries, e := os.ReadDir(dir)
	if e != nil {
		if os.IsNotExist(e) {
			return []any{}, nil
		}
		return nil, e
	}
	out := []any{}
	for _, v := range entries {
		if v.IsDir() || !strings.HasSuffix(v.Name(), ".db") {
			continue
		}
		fi, e := v.Info()
		if e != nil {
			continue
		}
		out = append(out, Object{"name": v.Name(), "path": filepath.Join(dir, v.Name()),
			"bytes": fi.Size(), "modified_at": fi.ModTime().UnixMilli()})
	}
	sort.Slice(out, func(i, j int) bool {
		return str(obj(out[i]), "name") > str(obj(out[j]), "name")
	})
	return out, nil
}

func (s *Store) Config() Config { return *s.snapshot.Load() }
func cloneConfig(c Config) Config {
	out := c
	out.Providers = append([]Provider{}, c.Providers...)
	out.Models = append([]Model{}, c.Models...)
	out.Routes = append([]Route{}, c.Routes...)
	for i := range out.Routes {
		out.Routes[i].Candidates = append([]Candidate{}, c.Routes[i].Candidates...)
	}
	out.Aliases = append([]Alias{}, c.Aliases...)
	return out
}

// sortConfig 按 Sort 升序排列供应商与路由，Sort 相同的维持原有的 ID 字母序。
// 加载与写入两条路径都要调用：只排加载那一条，界面在重启前看到的仍是旧顺序。
func sortConfig(c *Config) {
	sort.SliceStable(c.Providers, func(i, j int) bool { return c.Providers[i].Sort < c.Providers[j].Sort })
	sort.SliceStable(c.Routes, func(i, j int) bool { return c.Routes[i].Sort < c.Routes[j].Sort })
}

// applyOrder 按 rank 写入供应商或路由的 Sort，未列出的保持原值
func applyOrder(c *Config, providers bool, rank map[string]int) {
	if providers {
		for i := range c.Providers {
			if n, ok := rank[c.Providers[i].ID]; ok {
				c.Providers[i].Sort = n
			}
		}
		return
	}
	for i := range c.Routes {
		if n, ok := rank[c.Routes[i].ID]; ok {
			c.Routes[i].Sort = n
		}
	}
}

// enableModels 启用路由候选里的模型
func enableModels(c *Config, cs []Candidate) {
	for _, x := range cs {
		for i := range c.Models {
			if c.Models[i].ID == x.ModelID {
				c.Models[i].Enabled = true
			}
		}
	}
}

func (s *Store) Change(version int64, action, target string, fn func(*Config) error) (Config, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := cloneConfig(s.Config())
	if version != c.Version {
		return c, fail("VERSION_CONFLICT", "配置已更新，请刷新后重试", 409)
	}
	if e := fn(&c); e != nil {
		return c, e
	}
	if e := c.Validate(); e != nil {
		return c, fail("INVALID_CONFIG", e.Error(), 400)
	}
	sortConfig(&c)
	c.Version++
	e := s.DB.Transaction(func(t *sqlite.Tx) error {
		for _, table := range []string{"route_models", "aliases", "routes", "models", "providers", "settings"} {
			if e := t.Exec("DELETE FROM " + table); e != nil {
				return e
			}
		}
		for _, p := range c.Providers {
			p.HasKey = p.Secret != ""
			p.HasAdminKey = p.AdminSecret != ""
			if e := t.Exec("INSERT INTO providers(id,body,secret,admin_secret) VALUES (?,?,?,?)", p.ID, raw(p), s.encrypt(p.Secret), s.encrypt(p.AdminSecret)); e != nil {
				return e
			}
		}
		for _, m := range c.Models {
			if e := t.Exec("INSERT INTO models VALUES (?,?,?)", m.ID, m.ProviderID, raw(m)); e != nil {
				return e
			}
		}
		for _, r := range c.Routes {
			if e := t.Exec("INSERT INTO routes VALUES (?,?)", r.ID, raw(r)); e != nil {
				return e
			}
			for i, v := range r.Candidates {
				if e := t.Exec("INSERT INTO route_models VALUES (?,?,?,?)", r.ID, v.ModelID, i, v.Weight); e != nil {
					return e
				}
			}
		}
		for _, a := range c.Aliases {
			if e := t.Exec("INSERT INTO aliases VALUES (?,?)", a.ID, raw(a)); e != nil {
				return e
			}
		}
		var sm Object
		json.Unmarshal([]byte(raw(c.Settings)), &sm)
		for k, v := range sm {
			if e := t.Exec("INSERT INTO settings VALUES (?,?)", k, raw(v)); e != nil {
				return e
			}
		}
		if e := t.Exec("UPDATE meta SET value=? WHERE key='config_version'", fmt.Sprint(c.Version)); e != nil {
			return e
		}
		return t.Exec("INSERT INTO audit_logs VALUES (?,?,?,?,?)", randomID("audit_"), action, target, c.Version, now())
	})
	if e != nil {
		return s.Config(), e
	}
	s.snapshot.Store(&c)
	applyLogSettings(c.Settings)
	return c, nil
}
func (s *Store) AdminToken(reset bool) (string, error) {
	r, e := s.DB.Query("SELECT value FROM meta WHERE key='admin_digest'")
	if e != nil {
		return "", e
	}
	if len(r) > 0 && !reset {
		return "", nil
	}
	token := randomID("prism_admin_")
	e = s.DB.Transaction(func(t *sqlite.Tx) error {
		if e := t.Exec("INSERT INTO meta VALUES ('admin_digest',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", digest(token)); e != nil {
			return e
		}
		return t.Exec("DELETE FROM admin_sessions")
	})
	return token, e
}
func (s *Store) CheckAdmin(token string) bool {
	if len(token) < 24 {
		return false
	}
	r, e := s.DB.Query("SELECT value FROM meta WHERE key='admin_digest' AND value=?", digest(token))
	return e == nil && len(r) == 1
}
