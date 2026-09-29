package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 原生透传要重新序列化请求体：数字必须按原字面量到达上游，
// 不能被 float64 改写（大整数丢精度、1.0 变成 1）。
func TestNativePassthroughPreservesNumberLiterals(t *testing.T) {
	h := newHarness(t)
	var got string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = string(b)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"x","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer up.Close()
	h.configure(t, up.URL, modelFixture("m", "chat"))

	body := `{"model":"m","messages":[{"role":"user","content":"hi"}],"max_tokens":256,"seed":9007199254740993,"temperature":1.0,"logit_bias":{"50256":-100}}`
	r := httptest.NewRequest("POST", pathFor("chat"), strings.NewReader(body))
	w := httptest.NewRecorder()
	h.a.Engine.Handle(w, r, "chat", Principal{ID: "k"})
	requireStatus(t, w, 200)
	for _, want := range []string{`"seed":9007199254740993`, `"temperature":1.0`, `"50256":-100`} {
		if !strings.Contains(got, want) {
			t.Fatalf("上游收到的请求体应保留 %s，实得 %s", want, got)
		}
	}
}

// 换成 UseNumber 后，请求体仍然必须是单个合法 JSON 对象：尾随内容与非对象照旧拒绝。
func TestRequestBodyStillStrictJSON(t *testing.T) {
	h := newHarness(t)
	h.configure(t, "http://127.0.0.1:1", modelFixture("m", "chat"))
	for _, body := range []string{
		`{"model":"m","messages":[]} trailing`,
		`{"model":"m"}{"model":"m"}`,
		`[1,2,3]`,
		`null`,
		`{"model":`,
	} {
		r := httptest.NewRequest("POST", pathFor("chat"), strings.NewReader(body))
		w := httptest.NewRecorder()
		h.a.Engine.Handle(w, r, "chat", Principal{ID: "k"})
		if w.Code != 400 || !strings.Contains(w.Body.String(), "INVALID_JSON") {
			t.Fatalf("%q 应返回 400 INVALID_JSON，实得 %d %s", body, w.Code, w.Body)
		}
	}
}

// 数字以 json.Number 进入协议转换与输出上限钳制：数值语义不变，字面量照常转发。
func TestNumbersSurviveConversionAndClamping(t *testing.T) {
	h := newHarness(t)
	var got Object
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = Object{}
		json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/messages") {
			io.WriteString(w, `{"id":"x","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
			return
		}
		io.WriteString(w, `{"id":"x","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer up.Close()
	call := func(model, entry, body string) {
		t.Helper()
		r := httptest.NewRequest("POST", pathFor(entry), strings.NewReader(body))
		w := httptest.NewRecorder()
		h.a.Engine.Handle(w, r, entry, Principal{ID: "k"})
		requireStatus(t, w, 200)
	}

	h.configure(t, up.URL, modelFixture("chatm", "chat"), modelFixture("msgm", "messages"))
	// Messages 入口 -> chat 模型（跨协议）：采样参数与输出上限带着数值语义过去
	call("chatm", "messages", `{"model":"chatm","messages":[{"role":"user","content":"hi"}],"max_tokens":100,"temperature":0.2,"top_p":0.9}`)
	if num(got, "max_tokens") != 100 || num(got, "temperature") != 0.2 || num(got, "top_p") != 0.9 {
		t.Fatalf("跨协议后数值应不变: %v", got)
	}
	// 原生 Messages：输出上限超过模型上限时钳到上限，思考预算随之收紧
	call("msgm", "messages", `{"model":"msgm","messages":[{"role":"user","content":"hi"}],"max_tokens":64000,"thinking":{"type":"enabled","budget_tokens":32000}}`)
	if num(got, "max_tokens") != 4096 || num(obj(got["thinking"]), "budget_tokens") != 4095 {
		t.Fatalf("应钳制到模型上限 4096 / 4095: %v", got)
	}
}
