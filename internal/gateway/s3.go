package gateway

// 最小 S3 客户端：Put / Get / List / Delete，用于把备份放到 Cloudflare R2（也兼容 AWS S3、MinIO）。
// 签名是 AWS SigV4，用标准库实现，不引入 SDK。统一用路径风格 endpoint/bucket/key，R2 与 MinIO 都支持。
// 对象键只允许 [A-Za-z0-9._/-]，由调用方保证，签名时仍按 SigV4 规则转义。

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

type s3Client struct {
	Endpoint, Bucket, Region, AccessKey, Secret string
	HTTP                                        *http.Client
}

type s3Object struct {
	Key          string    `xml:"Key" json:"key"`
	Size         int64     `xml:"Size" json:"size"`
	LastModified time.Time `xml:"LastModified" json:"modified_at"`
	At           int64     `xml:"-" json:"at"` // 备份时刻（毫秒），取自对象名里的 UTC 时间
}

func hmacSHA256(key []byte, s string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(s))
	return m.Sum(nil)
}

func sha256Hex(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

// awsEscape 按 SigV4 规则转义：只保留 A-Z a-z 0-9 - _ . ~，路径里另外保留 /
func awsEscape(s string, path bool) string {
	var b strings.Builder
	for _, c := range []byte(s) {
		switch {
		case 'A' <= c && c <= 'Z', 'a' <= c && c <= 'z', '0' <= c && c <= '9', c == '-', c == '_', c == '.', c == '~', path && c == '/':
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// sign 给请求加上 SigV4 签名。签名覆盖 host 与全部 x-amz-* 头以及调用方显式设置的其他头。
func (c s3Client) sign(req *http.Request, payloadHash string, t time.Time) {
	amzDate, day := t.UTC().Format("20060102T150405Z"), t.UTC().Format("20060102")
	req.Header.Set("x-amz-date", amzDate)
	req.Header.Set("x-amz-content-sha256", payloadHash)
	headers := map[string]string{"host": req.URL.Host}
	for k, v := range req.Header {
		headers[strings.ToLower(k)] = strings.TrimSpace(strings.Join(v, ","))
	}
	names := make([]string, 0, len(headers))
	for k := range headers {
		names = append(names, k)
	}
	sort.Strings(names)
	var canonHeaders strings.Builder
	for _, k := range names {
		canonHeaders.WriteString(k + ":" + headers[k] + "\n")
	}
	signed := strings.Join(names, ";")
	q := req.URL.Query()
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	pairs := []string{}
	for _, k := range keys {
		for _, v := range q[k] {
			pairs = append(pairs, awsEscape(k, false)+"="+awsEscape(v, false))
		}
	}
	canonical := strings.Join([]string{req.Method, awsEscape(req.URL.Path, true), strings.Join(pairs, "&"), canonHeaders.String(), signed, payloadHash}, "\n")
	scope := day + "/" + c.Region + "/s3/aws4_request"
	toSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + sha256Hex([]byte(canonical))
	key := hmacSHA256(hmacSHA256(hmacSHA256(hmacSHA256([]byte("AWS4"+c.Secret), day), c.Region), "s3"), "aws4_request")
	req.Header.Set("Authorization", fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		c.AccessKey, scope, signed, hex.EncodeToString(hmacSHA256(key, toSign))))
}

func (c s3Client) do(ctx context.Context, method, key string, query url.Values, body []byte) ([]byte, error) {
	path := "/" + c.Bucket
	if key != "" {
		path += "/" + key
	}
	u, err := url.Parse(strings.TrimRight(c.Endpoint, "/") + path)
	if err != nil {
		return nil, err
	}
	u.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, method, u.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	c.sign(req, sha256Hex(body), time.Now())
	res, err := c.HTTP.Do(req)
	if err != nil {
		// 错误里的 URL 不含凭证，可以原样回报
		return nil, fmt.Errorf("连接存储失败：%w", err)
	}
	defer res.Body.Close()
	// 备份对象有上限：解压前 1 GB 足够覆盖可预见的库大小
	data, err := io.ReadAll(io.LimitReader(res.Body, 1<<30))
	if err != nil {
		return nil, err
	}
	if res.StatusCode >= 300 {
		var e struct {
			Code    string `xml:"Code"`
			Message string `xml:"Message"`
		}
		xml.Unmarshal(data, &e)
		return nil, fmt.Errorf("存储返回 HTTP %d %s %s", res.StatusCode, e.Code, e.Message)
	}
	return data, nil
}

func (c s3Client) put(ctx context.Context, key string, body []byte) error {
	_, err := c.do(ctx, "PUT", key, nil, body)
	return err
}

func (c s3Client) get(ctx context.Context, key string) ([]byte, error) {
	return c.do(ctx, "GET", key, nil, nil)
}

func (c s3Client) remove(ctx context.Context, key string) error {
	_, err := c.do(ctx, "DELETE", key, nil, nil)
	return err
}

// list 列出前缀下的全部对象（自动翻页）
func (c s3Client) list(ctx context.Context, prefix string) ([]s3Object, error) {
	out, token := []s3Object{}, ""
	for {
		q := url.Values{"list-type": {"2"}, "prefix": {prefix}}
		if token != "" {
			q.Set("continuation-token", token)
		}
		data, err := c.do(ctx, "GET", "", q, nil)
		if err != nil {
			return nil, err
		}
		var r struct {
			Contents  []s3Object `xml:"Contents"`
			Truncated bool       `xml:"IsTruncated"`
			Next      string     `xml:"NextContinuationToken"`
		}
		if err = xml.Unmarshal(data, &r); err != nil {
			return nil, fmt.Errorf("无法解析对象列表：%w", err)
		}
		out = append(out, r.Contents...)
		if !r.Truncated || r.Next == "" {
			return out, nil
		}
		token = r.Next
	}
}
