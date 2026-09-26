package gateway

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// AWS 官方 SigV4 样例（S3 文档「Signature Calculations for the Authorization Header」）
func TestS3SignatureVectors(t *testing.T) {
	c := s3Client{Region: "us-east-1", AccessKey: "AKIAIOSFODNN7EXAMPLE", Secret: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"}
	at := time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC)
	empty := sha256Hex(nil)
	cases := []struct {
		url, rng, want string
	}{
		{"https://examplebucket.s3.amazonaws.com/test.txt", "bytes=0-9", "f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41"},
		{"https://examplebucket.s3.amazonaws.com/?max-keys=2&prefix=J", "", "34b48302e7b5fa45bde8084f4b7868a86f0a534bc59db6670ed5711ef69dc6f7"},
	}
	for _, x := range cases {
		req, _ := http.NewRequest("GET", x.url, nil)
		if x.rng != "" {
			req.Header.Set("Range", x.rng)
		}
		c.sign(req, empty, at)
		if got := req.Header.Get("Authorization"); !strings.HasSuffix(got, "Signature="+x.want) {
			t.Errorf("%s 签名不对: %s", x.url, got)
		}
	}
}
