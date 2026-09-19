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
	"sync"
	"sync/atomic"
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
			`CREATE TABLE IF NOT EXISTS requests (id TEXT PRIMARY KEY, parent_id TEXT NOT NULL, key_id TEXT NOT NULL, requested_model TEXT NOT NULL, model_id TEXT NOT NULL, provider_id TEXT NOT NULL, protocol TEXT NOT NULL, upstream_protocol TEXT NOT NULL, session_id TEXT NOT NULL, status TEXT NOT NULL, http_status INTEGER NOT NULL DEFAULT 0, started_at INTEGER NOT NULL, duration_ms INTEGER NOT NULL DEFAULT 0, input_tokens INTEGER NOT NULL DEFAULT 0, output_tokens INTEGER NOT NULL DEFAULT 0, cache_tokens INTEGER NOT NULL DEFAULT 0, write_tokens INTEGER NOT NULL DEFAULT 0, cost_nano INTEGER NOT NULL DEFAULT 0, cost_known INTEGER NOT NULL DEFAULT 0, usage_mode TEXT NOT NULL DEFAULT 'reserved', reason TEXT NOT NULL, error_code TEXT NOT NULL DEFAULT '', is_demo INTEGER NOT NULL DEFAULT 0)`,
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
	r, e = s.DB.Query("SELECT body,secret FROM providers ORDER BY id")
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
	return nil
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
	c.Version++
	e := s.DB.Transaction(func(t *sqlite.Tx) error {
		for _, table := range []string{"route_models", "aliases", "routes", "models", "providers", "settings"} {
			if e := t.Exec("DELETE FROM " + table); e != nil {
				return e
			}
		}
		for _, p := range c.Providers {
			p.HasKey = p.Secret != ""
			if e := t.Exec("INSERT INTO providers VALUES (?,?,?)", p.ID, raw(p), s.encrypt(p.Secret)); e != nil {
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
