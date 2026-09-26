package gateway

// 流式输出的实时速度：每个上游流式片段到达时，把其中的正文、思考与工具参数字符数计入
// 该模型的逐秒窗口。请求结束才写 output_tokens，长回答在结束前完全看不到；
// 这里按约 3 个字符一个 token 估算，只用于实时面板，记账仍以上游报告的用量为准。

const charsPerToken = 3

// streamWindow 是 60 个逐秒桶，按秒号取模复用
type streamWindow [60]struct {
	sec   int64
	chars int
}

// deltaChars 取一个流式事件里新增输出的字符数
func deltaChars(o Object, p, typ string) int {
	runes := func(s string) int { return len([]rune(s)) }
	n := 0
	switch p {
	case "chat":
		for _, v := range arr(o["choices"]) {
			d := obj(obj(v)["delta"])
			n += runes(str(d, "content")) + runes(str(d, "reasoning_content")) + runes(str(d, "reasoning"))
			for _, tc := range arr(d["tool_calls"]) {
				n += runes(str(obj(obj(tc)["function"]), "arguments"))
			}
		}
	case "messages":
		if typ == "content_block_delta" {
			d := obj(o["delta"])
			n += runes(str(d, "text")) + runes(str(d, "thinking")) + runes(str(d, "partial_json"))
		}
	case "responses":
		if len(typ) > 6 && typ[len(typ)-6:] == ".delta" {
			n += runes(str(o, "delta"))
		}
	}
	return n
}

// addStreamed 把一个片段的字符数计入模型当前这一秒
func (e *Engine) addStreamed(model string, n int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.streamed == nil {
		e.streamed = map[string]*streamWindow{}
	}
	w := e.streamed[model]
	if w == nil {
		w = &streamWindow{}
		e.streamed[model] = w
	}
	sec := now() / 1000
	b := &w[sec%60]
	if b.sec != sec {
		b.sec, b.chars = sec, 0
	}
	b.chars += n
}

// streamedTokS 返回模型近 60 秒流式输出的估算 tok/s。调用方持有 e.mu
func (e *Engine) streamedTokS(model string, t int64) float64 {
	w := e.streamed[model]
	if w == nil {
		return 0
	}
	chars := 0
	for _, b := range w {
		if b.sec > t/1000-60 {
			chars += b.chars
		}
	}
	return float64(chars) / charsPerToken / 60
}
