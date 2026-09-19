package gateway

import (
	"encoding/json"
	"strings"
	"testing"
)

// 模糊测试的目标不是找 panic 之外的东西：这些函数处理的是上游和客户端送来的
// 任意 JSON / SSE 字节流，任何输入都必须走到「正常返回」或「明确报错」，
// 绝不能 panic 或无限循环。

func FuzzDecodeCanonical(f *testing.F) {
	for _, seed := range []string{
		`{"model":"m","messages":[{"role":"user","content":"hi"}]}`,
		`{"model":"m","input":"hi","max_output_tokens":16}`,
		`{"model":"m","max_tokens":8,"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`,
		`{"model":"m","messages":[{"role":"assistant","tool_calls":[{"id":"t1","type":"function","function":{"name":"f","arguments":"{}"}}]}]}`,
		`{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]}]}`,
		`{}`, `{"messages":null}`, `{"messages":[{}]}`,
	} {
		for _, p := range []string{"chat", "responses", "messages"} {
			f.Add(seed, p)
		}
	}
	f.Fuzz(func(t *testing.T, body, protocol string) {
		if protocol != "chat" && protocol != "responses" && protocol != "messages" {
			return
		}
		var o Object
		if json.Unmarshal([]byte(body), &o) != nil || o == nil {
			return
		}
		c, err := decodeCanonical(o, protocol)
		if err != nil {
			return
		}
		// 解出来的规范结构必须能再编码回三种协议而不 panic
		for _, target := range []string{"chat", "responses", "messages"} {
			if _, err := encodeCanonical(c, target, "m"); err != nil {
				continue
			}
		}
	})
}

func FuzzDecodeCompletion(f *testing.F) {
	for _, seed := range []string{
		`{"choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":2}}`,
		`{"content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":2}}`,
		`{"output":[{"type":"message","content":[{"type":"output_text","text":"hi"}]}],"usage":{"input_tokens":1,"output_tokens":1}}`,
		`{"choices":[]}`, `{"content":[]}`, `null`, `{"usage":{"input_tokens":-1}}`,
	} {
		for _, p := range []string{"chat", "responses", "messages"} {
			f.Add(seed, p)
		}
	}
	f.Fuzz(func(t *testing.T, body, protocol string) {
		if protocol != "chat" && protocol != "responses" && protocol != "messages" {
			return
		}
		var o Object
		if json.Unmarshal([]byte(body), &o) != nil || o == nil {
			return
		}
		c, err := decodeCompletion(o, protocol)
		if err != nil {
			return
		}
		for _, target := range []string{"chat", "responses", "messages"} {
			completionObject(c, target, "m", "req_1")
			stopFor(c, target)
		}
		var u Usage
		extractUsage(o, protocol, &u)
		if u.Input < 0 || u.Output < 0 || u.Cache < 0 || u.Write < 0 {
			t.Fatalf("用量不应为负: %+v (输入 %q)", u, body)
		}
	})
}

func FuzzReadSSE(f *testing.F) {
	for _, seed := range []string{
		"data: {\"a\":1}\n\n",
		"event: message_stop\ndata: {}\n\n",
		"data: [DONE]\n\n",
		"data: line1\ndata: line2\n\n",
		": comment\n\ndata: {}\n\n",
		"data:", "\n\n\n", "event:\ndata:\n\n",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, body string) {
		if len(body) > 1<<20 {
			return
		}
		frames := 0
		err := readSSE(strings.NewReader(body), func(fr frame) error {
			frames++
			if frames > 100000 {
				t.Fatal("帧数异常，可能存在空转")
			}
			// Raw 必须始终是输入的一部分，不能凭空生成内容
			if fr.Data != "" && !strings.Contains(fr.Raw, strings.Split(fr.Data, "\n")[0]) {
				t.Fatalf("帧数据与原始文本不一致: %q vs %q", fr.Data, fr.Raw)
			}
			return nil
		})
		_ = err
	})
}
