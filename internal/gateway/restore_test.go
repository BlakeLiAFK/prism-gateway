package gateway

import (
	"bytes"
	"compress/gzip"
	"encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeS3 是路径风格的内存 S3：Put / Get / ListObjectsV2 / Delete，要求带 SigV4 签名头
type fakeS3 struct {
	mu   sync.Mutex
	objs map[string][]byte
}

func newFakeS3(t *testing.T) (*fakeS3, *httptest.Server) {
	f := &fakeS3{objs: map[string][]byte{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 Credential=AKTEST/") || r.Header.Get("x-amz-content-sha256") == "" {
			w.WriteHeader(403)
			return
		}
		key := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/bkt"), "/")
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case r.Method == "PUT":
			f.objs[key], _ = io.ReadAll(r.Body)
		case r.Method == "DELETE":
			delete(f.objs, key)
			w.WriteHeader(204)
		case r.Method == "GET" && key == "":
			type obj struct {
				Key          string
				Size         int
				LastModified string
			}
			var res struct {
				XMLName  xml.Name `xml:"ListBucketResult"`
				Contents []obj
			}
			keys := []string{}
			for k := range f.objs {
				if strings.HasPrefix(k, r.URL.Query().Get("prefix")) {
					keys = append(keys, k)
				}
			}
			sort.Strings(keys)
			for _, k := range keys {
				res.Contents = append(res.Contents, obj{k, len(f.objs[k]), time.Now().UTC().Format(time.RFC3339)})
			}
			xml.NewEncoder(w).Encode(res)
		case r.Method == "GET":
			if b, ok := f.objs[key]; ok {
				w.Write(b)
				return
			}
			w.WriteHeader(404)
			io.WriteString(w, `<Error><Code>NoSuchKey</Code><Message>missing</Message></Error>`)
		}
	}))
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeS3) keys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []string{}
	for k := range f.objs {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func r2Harness(t *testing.T) (*harness, *fakeS3) {
	h := newHarness(t)
	f, srv := newFakeS3(t)
	w, out := h.rpc(t, "r2.save", Object{"endpoint": srv.URL, "bucket": "bkt", "prefix": "prism/", "access_key_id": "AKTEST", "secret": "s3cr3t"}, h.token)
	requireStatus(t, w, 200)
	if d := obj(out["data"]); d["has_secret"] != true || d["configured"] != true || strings.Contains(raw(d), "s3cr3t") {
		t.Fatalf("R2 配置不应回显密钥: %v", d)
	}
	return h, f
}

// 远程备份：内存快照压缩上传；只保留设定份数；列表只含网关上传的对象
func TestRemoteBackupRotation(t *testing.T) {
	h, f := r2Harness(t)
	requireStatus(t, func() *httptest.ResponseRecorder { w, _ := h.rpc(t, "r2.test", Object{}, h.token); return w }(), 200)
	f.objs["prism/other.txt"] = []byte("不属于网关的对象")
	h.rpc(t, "schedule.save", Object{"remote_keep": 2}, h.token)
	for i := 0; i < 3; i++ {
		if r := runScheduled(t, h, "remote"); str(r, "status") != "succeeded" {
			t.Fatalf("远程备份失败: %v", r)
		}
		time.Sleep(3 * time.Millisecond)
	}
	keys := f.keys()
	if len(keys) != 3 || keys[2] != "prism/other.txt" {
		t.Fatalf("应保留 2 份备份与桶里的其他对象: %v", keys)
	}
	zr, err := gzip.NewReader(bytes.NewReader(f.objs[keys[0]]))
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := io.ReadAll(zr); !bytes.HasPrefix(b, []byte("SQLite format 3\x00")) {
		t.Fatal("上传的应是 gzip 压缩的 SQLite 数据库")
	}
	_, out := h.rpc(t, "backup.remote_list", Object{}, h.token)
	if l := arr(out["data"]); len(l) != 2 || str(obj(l[0]), "key") != keys[1] {
		t.Fatalf("列表应只含 2 份网关备份且新的在前: %v", l)
	}
}

// 从 R2 还原：配置回到备份时的样子；管理员令牌、R2 配置沿用当前值；生成安全快照
func TestRestoreFromRemote(t *testing.T) {
	h, f := r2Harness(t)
	h.configure(t, "http://127.0.0.1:1", modelFixture("keep", "chat"))
	runScheduled(t, h, "remote")
	h.change(t, func(c *Config) { c.Models = append(c.Models, modelFixture("later", "chat")) })
	// 备份之后轮换了管理员令牌：还原后必须仍用新令牌，否则管理员会被锁在外面
	oldToken := h.token
	token, err := h.s.AdminToken(true)
	if err != nil {
		t.Fatal(err)
	}
	h.token = token
	key := f.keys()[0]
	for _, bad := range []Object{{"source": "remote", "name": key}, {"source": "remote", "name": "prism/other.db.gz", "confirm": true}, {"source": "local", "name": "../gateway.db", "confirm": true}} {
		if w, _ := h.rpc(t, "backup.restore", bad, h.token); w.Code != 400 {
			t.Fatalf("%v 应被拒绝: %d", bad, w.Code)
		}
	}
	w, out := h.rpc(t, "backup.restore", Object{"source": "remote", "name": key, "confirm": true}, h.token)
	requireStatus(t, w, 200)
	d := obj(out["data"])
	if _, ok := h.s.Config().model("later"); ok {
		t.Fatal("还原后不应再有备份之后添加的模型")
	}
	if _, ok := h.s.Config().model("keep"); !ok {
		t.Fatal("还原后应有备份里的模型")
	}
	if _, err := os.Stat(filepath.Join(h.s.backupDir(), str(d, "safety_backup"))); err != nil {
		t.Fatalf("应生成安全快照: %v", d)
	}
	// 新管理员令牌仍然有效、旧令牌无效，R2 配置仍在
	if w, _ := h.rpc(t, "r2.get", Object{}, oldToken); w.Code == 200 {
		t.Fatal("备份里的旧管理员令牌不应在还原后复活")
	}
	w, out = h.rpc(t, "r2.get", Object{}, h.token)
	requireStatus(t, w, 200)
	if obj(out["data"])["configured"] != true {
		t.Fatal("还原后 R2 配置应沿用当前值")
	}
}

// 本机备份还原；主密钥不同的备份被拒绝且线上库不受影响
func TestRestoreLocalAndKeyMismatch(t *testing.T) {
	h, f := r2Harness(t)
	h.configure(t, "http://127.0.0.1:1", modelFixture("keep", "chat"))
	_, out := h.rpc(t, "backup.create", Object{}, h.token)
	name := filepath.Base(str(obj(out["data"]), "path"))
	h.change(t, func(c *Config) { c.Models = nil })
	requireStatus(t, func() *httptest.ResponseRecorder {
		w, _ := h.rpc(t, "backup.restore", Object{"source": "local", "name": name, "confirm": true}, h.token)
		return w
	}(), 200)
	if _, ok := h.s.Config().model("keep"); !ok {
		t.Fatal("本机备份还原后应有模型 keep")
	}
	// 另一套主密钥的库：上游凭证解不开，必须拒绝
	other := newHarness(t)
	other.configure(t, "http://127.0.0.1:1", modelFixture("alien", "chat"))
	var buf bytes.Buffer
	other.s.DB.Snapshot(func(b []byte) error { zw := gzip.NewWriter(&buf); zw.Write(b); return zw.Close() })
	f.objs["prism/gateway-20990101-000000-000.db.gz"] = buf.Bytes()
	w, out := h.rpc(t, "backup.restore", Object{"source": "remote", "name": "prism/gateway-20990101-000000-000.db.gz", "confirm": true}, h.token)
	if w.Code != 400 || !strings.Contains(raw(out), ".key") {
		t.Fatalf("主密钥不匹配应拒绝并提示 .key: %d %v", w.Code, out)
	}
	if _, ok := h.s.Config().model("keep"); !ok {
		t.Fatal("被拒绝的还原不应改动线上库")
	}
}

// 备份时刻取自对象名里的 UTC 时间，不依赖存储返回的 LastModified
func TestBackupTimeFromKey(t *testing.T) {
	got := backupTime(s3Object{Key: "prism/gateway-20260926-085611-422.db.gz"})
	if want := time.Date(2026, 9, 26, 8, 56, 11, 422e6, time.UTC).UnixMilli(); got != want {
		t.Fatalf("got %d want %d", got, want)
	}
	lm := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if got = backupTime(s3Object{Key: "prism/gateway-bad.db.gz", LastModified: lm}); got != lm.UnixMilli() {
		t.Fatalf("解析失败应退回 LastModified: %d", got)
	}
}
