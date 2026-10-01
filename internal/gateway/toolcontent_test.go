package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestToolResultContentConversion(t *testing.T) {
	image := Block{Kind: "image", URL: toolImagePNG}
	for _, tc := range []struct {
		name    string
		result  Block
		wantLen int
		wantErr bool
	}{
		{name: "single image", result: Block{Kind: "result", ID: "one", Content: []Block{image}}, wantLen: 2},
		{name: "multiple images", result: Block{Kind: "result", ID: "many", Content: []Block{image, image}}, wantLen: 3},
		{name: "error image", result: Block{Kind: "result", ID: "failed", IsError: true, Content: []Block{{Kind: "text", Text: "failed"}, image}}, wantLen: 4, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := encodeCanonical(Canonical{Messages: []Message{{Role: "user", Blocks: []Block{tc.result}}}}, "chat", "m")
			if err != nil {
				t.Fatal(err)
			}
			messages := arr(out["messages"])
			if len(messages) != 2 || str(obj(messages[0]), "tool_call_id") != tc.result.ID {
				t.Fatalf("工具结果结构错误: %s", raw(messages))
			}
			content := arr(obj(messages[1])["content"])
			if len(content) != tc.wantLen || str(obj(messages[1]), "role") != "user" {
				t.Fatalf("工具图片数量或位置错误: %s", raw(messages))
			}
			if tc.wantErr && str(obj(content[1]), "text") != "[tool error]" {
				t.Fatalf("错误工具结果缺少标记: %s", raw(content))
			}
		})
	}

	out, err := encodeCanonical(Canonical{Messages: []Message{{Role: "user", Blocks: []Block{{Kind: "result", ID: "text", Text: "unchanged"}}}}}, "chat", "m")
	if err != nil {
		t.Fatal(err)
	}
	messages := arr(out["messages"])
	if len(messages) != 1 || str(obj(messages[0]), "content") != "unchanged" {
		t.Fatalf("纯文本工具结果原行为改变: %s", raw(messages))
	}
}

func TestResponsesToolImageConversion(t *testing.T) {
	body := Object{"model": "m", "input": []any{
		Object{"type": "function_call", "call_id": "call", "name": "read", "arguments": `{}`},
		Object{"type": "function_call_output", "call_id": "call", "output": []any{
			Object{"type": "input_text", "text": "caption"},
			Object{"type": "input_image", "image_url": toolImagePNG},
		}},
	}}
	canonical, err := decodeCanonical(body, "responses")
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"messages", "chat"} {
		t.Run(target, func(t *testing.T) {
			out, err := encodeCanonical(canonical, target, "m")
			if err != nil {
				t.Fatal(err)
			}
			encoded := raw(out)
			for _, want := range []string{"caption", strings.SplitN(toolImagePNG, ",", 2)[1]} {
				if strings.Count(encoded, want) != 1 {
					t.Fatalf("%s 转换丢失或重复 %q: %s", target, want, encoded)
				}
			}
		})
	}
}

func TestParallelToolImagesAcrossCanonicalMessages(t *testing.T) {
	canonical := Canonical{Messages: []Message{
		{Role: "user", Blocks: []Block{{Kind: "result", ID: "first", Content: []Block{{Kind: "image", URL: toolImagePNG}}}}},
		{Role: "user", Blocks: []Block{{Kind: "result", ID: "second", Content: []Block{{Kind: "text", Text: "second text"}, {Kind: "image", URL: "https://example.com/second.png"}}}}},
	}}
	out, err := encodeCanonical(canonical, "chat", "m")
	if err != nil {
		t.Fatal(err)
	}
	messages := arr(out["messages"])
	if len(messages) != 3 || str(obj(messages[0]), "tool_call_id") != "first" || str(obj(messages[1]), "tool_call_id") != "second" || str(obj(messages[2]), "role") != "user" {
		t.Fatalf("并行工具结果顺序错误: %s", raw(messages))
	}
	content := arr(obj(messages[2])["content"])
	if len(content) != 5 || !strings.Contains(str(obj(content[0]), "text"), "first") || !strings.Contains(str(obj(content[2]), "text"), "second") || str(obj(content[3]), "text") != "second text" {
		t.Fatalf("跨消息工具图片顺序错误: %s", raw(content))
	}
}

func TestToolResultContentRejectsUnsupportedAndInvalidImages(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content []any
	}{
		{name: "unsupported block", content: []any{Object{"type": "document", "source": Object{"type": "text", "data": "x"}}}},
		{name: "invalid image", content: []any{Object{"type": "image", "source": Object{"type": "base64", "media_type": "image/png", "data": "%%%"}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := decodeCanonical(Object{"model": "m", "messages": []any{Object{"role": "user", "content": []any{Object{"type": "tool_result", "tool_use_id": "call", "content": tc.content}}}}}, "messages")
			if err == nil {
				t.Fatal("应拒绝无效工具结果内容")
			}
		})
	}
}

func TestToolResultImageRequiresVision(t *testing.T) {
	h := newHarness(t)
	var hits atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_ = json.NewEncoder(w).Encode(Object{})
	}))
	defer up.Close()
	model := modelFixture("m", "chat")
	model.Vision = false
	h.configure(t, up.URL, model)
	w := h.generate(t, "messages", toolImageRequest())
	requireStatus(t, w, 400)
	if hits.Load() != 0 || !strings.Contains(w.Body.String(), "vision_unsupported") {
		t.Fatalf("无视觉能力的模型不应收到图片请求: hits=%d body=%s", hits.Load(), w.Body)
	}
}

func TestFallbackClearsToolImageAdaptedHeader(t *testing.T) {
	h := newHarness(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body Object
		_ = json.NewDecoder(r.Body).Decode(&body)
		if str(body, "model") == "upstream-first" {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer up.Close()
	h.configure(t, up.URL, modelFixture("first", "chat"), modelFixture("second", "messages"))
	h.change(t, func(c *Config) {
		c.Routes = []Route{{ID: "auto", Name: "auto", Enabled: true, Strategy: "priority", Candidates: []Candidate{{ModelID: "first", Weight: 10}, {ModelID: "second", Weight: 10}}}}
	})
	body := toolImageRequest()
	body["model"] = "auto"
	w := h.generate(t, "messages", body)
	requireStatus(t, w, 200)
	if got := w.Header().Get("X-Prism-Adapted"); got != "" {
		t.Fatalf("回退到原生候选后不应保留 X-Prism-Adapted: %q", got)
	}
}
