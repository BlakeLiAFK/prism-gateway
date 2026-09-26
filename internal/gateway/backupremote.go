package gateway

// 远程备份：定时把数据库快照直接从内存压缩上传到 R2（S3 兼容），本机磁盘零额外占用。
// 存储配置存 meta.r2_config，Secret Access Key 加密保存、接口只写不读。
// 上传的对象名固定为「前缀 + gateway-时间.db.gz」，轮转只清理这种对象，桶里的其他文件不动。
// 快照不含 .key 主密钥：换机器还原时必须带上原主密钥。

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"
)

type r2Config struct {
	Endpoint  string `json:"endpoint"`
	Bucket    string `json:"bucket"`
	Prefix    string `json:"prefix"`
	Region    string `json:"region"`
	AccessKey string `json:"access_key_id"`
	Secret    string `json:"secret"`
}

var (
	bucketName   = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
	objectPrefix = regexp.MustCompile(`^[A-Za-z0-9._/-]{0,100}$`)
)

const remoteObjectSuffix = ".db.gz"

func init() {
	consoleActions["r2.get"] = func(a *App, _ string, _ Object) (any, error) { return a.r2View() }
	consoleActions["r2.save"] = func(a *App, _ string, p Object) (any, error) { return a.saveR2(p) }
	consoleActions["r2.test"] = func(a *App, _ string, _ Object) (any, error) { return a.testR2() }
	consoleActions["backup.remote_list"] = func(a *App, _ string, _ Object) (any, error) { return a.remoteBackups() }
}

func (a *App) loadR2() (r2Config, error) {
	c := r2Config{Region: "auto", Prefix: "prism/"}
	rows, err := a.Store.DB.Query("SELECT value FROM meta WHERE key='r2_config'")
	if err != nil || len(rows) == 0 {
		return c, err
	}
	if err = json.Unmarshal([]byte(rows[0].String("value")), &c); err != nil {
		return c, err
	}
	if c.Secret != "" {
		if c.Secret, err = a.Store.decrypt(c.Secret); err != nil {
			return c, fmt.Errorf("无法解密存储凭证，请确认 .key 主密钥未更换: %w", err)
		}
	}
	return c, nil
}

func (a *App) r2View() (any, error) {
	c, err := a.loadR2()
	if err != nil {
		return nil, err
	}
	return Object{"endpoint": c.Endpoint, "bucket": c.Bucket, "prefix": c.Prefix, "region": c.Region, "access_key_id": c.AccessKey,
		"has_secret": c.Secret != "", "configured": c.ready()}, nil
}

func (c r2Config) ready() bool {
	return c.Endpoint != "" && c.Bucket != "" && c.AccessKey != "" && c.Secret != ""
}

func (a *App) saveR2(p Object) (any, error) {
	c, err := a.loadR2()
	if err != nil {
		return nil, err
	}
	c.Endpoint = strings.TrimRight(strings.TrimSpace(str(p, "endpoint")), "/")
	c.Bucket, c.Prefix = strings.TrimSpace(str(p, "bucket")), strings.TrimSpace(str(p, "prefix"))
	c.Region, c.AccessKey = strings.TrimSpace(str(p, "region")), strings.TrimSpace(str(p, "access_key_id"))
	if c.Region == "" {
		c.Region = "auto"
	}
	switch {
	case !strings.HasPrefix(c.Endpoint, "https://") && !strings.HasPrefix(c.Endpoint, "http://"):
		return nil, fail("INVALID_PARAMS", "Endpoint 需以 https:// 开头，R2 形如 https://<账户ID>.r2.cloudflarestorage.com", 400)
	case !bucketName.MatchString(c.Bucket):
		return nil, fail("INVALID_PARAMS", "存储桶名称只能包含小写字母、数字、点和连字符，长度 3–63", 400)
	case !objectPrefix.MatchString(c.Prefix) || strings.HasPrefix(c.Prefix, "/") || strings.Contains(c.Prefix, ".."):
		return nil, fail("INVALID_PARAMS", "对象前缀只能包含字母、数字与 . _ / -，不能以 / 开头", 400)
	case c.AccessKey == "":
		return nil, fail("INVALID_PARAMS", "请填写 Access Key ID", 400)
	}
	// Secret 留空表示沿用已保存的值
	if s := strings.TrimSpace(str(p, "secret")); s != "" {
		c.Secret = s
	}
	stored := c
	stored.Secret = a.Store.encrypt(c.Secret)
	if err = a.Store.DB.Exec("INSERT INTO meta VALUES ('r2_config', ?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", raw(stored)); err != nil {
		return nil, err
	}
	a.audit("r2.save", c.Bucket)
	return a.r2View()
}

func (a *App) r2Client() (s3Client, string, error) {
	c, err := a.loadR2()
	if err != nil {
		return s3Client{}, "", err
	}
	if !c.ready() {
		return s3Client{}, "", fail("R2_NOT_CONFIGURED", "尚未配置 R2 存储：请在「备份与还原」里填写 Endpoint、存储桶与访问密钥", 400)
	}
	return s3Client{Endpoint: c.Endpoint, Bucket: c.Bucket, Region: c.Region, AccessKey: c.AccessKey, Secret: c.Secret,
		HTTP: &http.Client{Timeout: 10 * time.Minute}}, c.Prefix, nil
}

// testR2 写入、列出、删除一个探测对象，三步都成功才算连通
func (a *App) testR2() (any, error) {
	cl, prefix, err := a.r2Client()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(a.Context, 30*time.Second)
	defer cancel()
	key := prefix + ".prism-probe"
	for _, step := range []struct {
		name string
		fn   func() error
	}{
		{"写入", func() error { return cl.put(ctx, key, []byte("ok")) }},
		{"列出", func() error { _, e := cl.list(ctx, prefix); return e }},
		{"删除", func() error { return cl.remove(ctx, key) }},
	} {
		if err = step.fn(); err != nil {
			return nil, fail("R2_FAILED", step.name+"失败："+err.Error(), 502)
		}
	}
	return Object{"ok": true}, nil
}

// remoteBackups 列出网关上传的备份，新的在前
func (a *App) remoteBackups() (any, error) {
	cl, prefix, err := a.r2Client()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(a.Context, 30*time.Second)
	defer cancel()
	objs, err := cl.list(ctx, prefix+"gateway-")
	if err != nil {
		return nil, fail("R2_FAILED", "列出备份失败："+err.Error(), 502)
	}
	out := []s3Object{}
	for _, o := range objs {
		if strings.HasSuffix(o.Key, remoteObjectSuffix) {
			o.At = backupTime(o)
			out = append(out, o)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key > out[j].Key })
	return out, nil
}

// taskRemote 生成内存快照、gzip 后上传，再按保留份数清理旧的远程备份
func (a *App) taskRemote(c scheduleConfig) (string, error) {
	cl, prefix, err := a.r2Client()
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	size := 0
	err = a.Store.DB.Snapshot(func(b []byte) error {
		size = len(b)
		zw := gzip.NewWriter(&buf)
		if _, e := zw.Write(b); e != nil {
			return e
		}
		return zw.Close()
	})
	if err != nil {
		return "", fmt.Errorf("生成快照失败：%w", err)
	}
	ctx, cancel := context.WithTimeout(a.Context, 10*time.Minute)
	defer cancel()
	// 带毫秒，与本机备份同样的命名，手动连续运行也不会撞名覆盖
	stamp := strings.Replace(time.Now().UTC().Format("20060102-150405.000"), ".", "-", 1)
	key := prefix + "gateway-" + stamp + remoteObjectSuffix
	if err = cl.put(ctx, key, buf.Bytes()); err != nil {
		return "", fmt.Errorf("上传失败：%w", err)
	}
	removed, err := a.pruneRemote(ctx, cl, prefix, c.RemoteKeep)
	if err != nil {
		return "", fmt.Errorf("已上传 %s，但清理旧备份失败：%w", key, err)
	}
	return fmt.Sprintf("已上传 %s（原始 %.1f MB，压缩后 %.1f MB），清理旧的远程备份 %d 份",
		key, float64(size)/(1<<20), float64(buf.Len())/(1<<20), removed), nil
}

func (a *App) pruneRemote(ctx context.Context, cl s3Client, prefix string, keep int) (int, error) {
	objs, err := cl.list(ctx, prefix+"gateway-")
	if err != nil {
		return 0, err
	}
	keys := []string{}
	for _, o := range objs {
		if strings.HasSuffix(o.Key, remoteObjectSuffix) {
			keys = append(keys, o.Key)
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(keys)))
	removed := 0
	for _, k := range keys[min(keep, len(keys)):] {
		if err = cl.remove(ctx, k); err != nil {
			return removed, err
		}
		removed++
	}
	return removed, nil
}

// backupTime 从对象名「gateway-20060102-150405-000.db.gz」解析备份时刻：
// 有的 S3 实现返回的 LastModified 时区不对，对象名里的 UTC 时间更可靠；解析不了才用 LastModified
func backupTime(o s3Object) int64 {
	stamp := strings.TrimSuffix(o.Key[strings.LastIndex(o.Key, "gateway-")+len("gateway-"):], remoteObjectSuffix)
	// 毫秒前的连字符换成小数点，才能按 .000 解析
	if i := strings.LastIndex(stamp, "-"); i > 0 {
		if t, err := time.Parse("20060102-150405.000", stamp[:i]+"."+stamp[i+1:]); err == nil {
			return t.UnixMilli()
		}
	}
	return o.LastModified.UnixMilli()
}
