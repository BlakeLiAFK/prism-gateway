package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const toolImagePNG = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+aX1sAAAAASUVORK5CYII="

func toolImageRequest() Object {
	return Object{"model": "m", "max_tokens": 128, "messages": []any{
		Object{"role": "user", "content": "Describe the tool image."},
		Object{"role": "assistant", "content": []any{
			Object{"type": "tool_use", "id": "image_call", "name": "read_file", "input": Object{"path": "image.png"}},
			Object{"type": "tool_use", "id": "text_call", "name": "read_file", "input": Object{"path": "notes.txt"}},
		}},
		Object{"role": "user", "content": []any{
			Object{"type": "tool_result", "tool_use_id": "image_call", "content": []any{
				Object{"type": "text", "text": "before image"},
				Object{"type": "image", "source": Object{"type": "base64", "media_type": "image/png", "data": strings.SplitN(toolImagePNG, ",", 2)[1]}},
				Object{"type": "text", "text": "after image"},
			}},
			Object{"type": "tool_result", "tool_use_id": "text_call", "content": "notes output"},
		}},
	}}
}

func TestToolResultImagesThroughHandler(t *testing.T) {
	for _, target := range []string{"chat", "responses", "messages"} {
		for _, stream := range []bool{false, true} {
			t.Run(target+map[bool]string{false: "/json", true: "/sse"}[stream], func(t *testing.T) {
				h := newHarness(t)
				received := make(chan Object, 1)
				up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					var got Object
					if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
						t.Error(err)
						w.WriteHeader(400)
						return
					}
					received <- got
					if stream {
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = w.Write([]byte(fixtureSSE(t, target)))
						return
					}
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(completionObject(Completion{Blocks: []Block{{Kind: "text", Text: "image received"}}}, target, "m", "fixture"))
				}))
				defer up.Close()
				h.configure(t, up.URL, modelFixture("m", target))
				body := toolImageRequest()
				body["stream"] = stream
				w := h.generate(t, "messages", body)
				requireStatus(t, w, 200)
				wantAdapted := ""
				if target == "chat" {
					wantAdapted = "tool-result-images"
				}
				if got := w.Header().Get("X-Prism-Adapted"); got != wantAdapted {
					t.Fatalf("X-Prism-Adapted=%q want=%q", got, wantAdapted)
				}
				assertToolImageRequest(t, <-received, target)
			})
		}
	}
}

func assertToolImageRequest(t *testing.T, got Object, target string) {
	t.Helper()
	for _, want := range []string{strings.SplitN(toolImagePNG, ",", 2)[1], "before image", "after image", "notes output"} {
		if strings.Count(raw(got), want) != 1 {
			t.Fatalf("内容必须完整且只出现一次: %q: %s", want, raw(got))
		}
	}
	if target == "chat" {
		messages := arr(got["messages"])
		if len(messages) != 5 || str(obj(messages[2]), "tool_call_id") != "image_call" || str(obj(messages[3]), "tool_call_id") != "text_call" || str(obj(messages[4]), "role") != "user" {
			t.Fatalf("图片必须排在全部并行工具结果之后: %s", raw(messages))
		}
		content := arr(obj(messages[4])["content"])
		if len(content) != 4 || !strings.Contains(str(obj(content[0]), "text"), "image_call") || str(obj(content[1]), "text") != "before image" || str(obj(obj(content[2])["image_url"]), "url") != toolImagePNG || str(obj(content[3]), "text") != "after image" {
			t.Fatalf("图片、顺序或工具关联丢失: %s", raw(content))
		}
	} else if target == "responses" {
		input := arr(got["input"])
		result := obj(input[3])
		content := arr(result["output"])
		if str(result, "call_id") != "image_call" || len(content) != 3 || str(obj(content[1]), "type") != "input_image" || str(obj(content[1]), "image_url") != toolImagePNG {
			t.Fatalf("Responses 工具结果图片未保留: %s", raw(input))
		}
	} else if raw(got["messages"]) != raw(toolImageRequest()["messages"]) {
		t.Fatal("原生图片工具结果被改写")
	}
}
