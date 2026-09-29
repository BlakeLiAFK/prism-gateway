package gateway

// Anthropic Messages 的 count_tokens 端点：有原生计数接口的模型转发给上游，其余按设置本地估算。
// 该端点只计数不生成，不经过 Key 的 RPM / 预算检查，也不写请求记录（见 docs/API.md）。

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

func (e *Engine) CountTokens(w http.ResponseWriter, r *http.Request, key Principal) {
	id := randomID("req_")
	var o Object
	r.Body = http.MaxBytesReader(w, r.Body, int64(e.store.Config().Settings.MaxBodyMB)<<20)
	dec := json.NewDecoder(r.Body)
	dec.UseNumber()
	if err := dec.Decode(&o); err != nil {
		protocolError(w, "messages", 400, "INVALID_JSON", "JSON 无效", id)
		return
	}
	if !key.allows(str(o, "model")) {
		protocolError(w, "messages", 403, "MODEL_FORBIDDEN", "模型未授权", id)
		return
	}
	c := e.store.Config()
	cp := Object{}
	for k, v := range o {
		cp[k] = v
	}
	cp["max_tokens"] = 1
	sel, _, err := e.selections(c, cp, "messages", "")
	if err != nil || len(sel) == 0 {
		protocolError(w, "messages", 400, "NO_COMPATIBLE_MODEL", "没有兼容模型", id)
		return
	}
	s := sel[0]
	if s.Model.NativeCount && s.Model.Protocol == "messages" && s.Provider.Kind != "mock" {
		body := Object{}
		for k, v := range o {
			body[k] = v
		}
		body["model"] = s.Model.Upstream
		req, _ := http.NewRequestWithContext(r.Context(), "POST", strings.TrimRight(s.Provider.BaseURL, "/")+"/messages/count_tokens", bytes.NewBufferString(raw(body)))
		requestHeaders(req, r, s.Provider, "messages", "")
		res, err := e.client(s.Provider).Do(req)
		if err != nil {
			protocolError(w, "messages", 502, "UPSTREAM_ERROR", "计数上游连接失败", id)
			return
		}
		defer res.Body.Close()
		data, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
		if err == nil && res.StatusCode == 200 && json.Valid(data) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-Prism-Token-Count-Mode", "provider")
			w.Write(data)
			return
		}
		protocolError(w, "messages", 502, "UPSTREAM_ERROR", "上游计数失败；没有静默替换为估算值", id)
		return
	}
	if !c.Settings.AllowEstimatedCount {
		protocolError(w, "messages", 501, "TOKEN_COUNT_UNSUPPORTED", "此模型没有原生计数接口；本地估算已关闭", id)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Prism-Token-Count-Mode", "estimated")
	json.NewEncoder(w).Encode(Object{"input_tokens": estimateInput(o)})
}
