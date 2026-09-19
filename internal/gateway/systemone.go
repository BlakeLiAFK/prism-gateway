package gateway

import (
	"errors"
	"fmt"
	"math"
	"sort"
)

// TypeSafe 的 System One 协议：上游拿一份状态和若干个带类型的问题，
// 返回类型化答案与概率分布，而不是文本。
//
// 它和 chat / responses / messages 没有可以互相映射的语义——那三种协议的
// 载荷是消息序列与文本增量，这里是状态加问题、答案加概率。硬转换只能靠
// 编造文本或者把 JSON 塞进 content 字符串，两者都会静默改变调用方拿到的
// 东西。所以它单独成面，engine 里显式禁止与对话协议互转。

// 三种题型的名字，与上游 type 字段一致。
const (
	qNoul   = "noul"   // 是非判断，返回「是」的概率
	qChoice = "choice" // 从给定选项里选一个
	qScore  = "score"  // 按有序分级打分
)

// validateSystemOne 在请求发往上游之前挡住结构性错误。
// 上游对这些同样会报 422，但那时候配额已经预留、请求记录也已经写下了；
// 而且它的错误正文我们不转发，调用方只会看到一句「上游请求失败」。
func validateSystemOne(o Object) error {
	if o["state"] == nil {
		return errors.New("必须提供 state：要评估的内容，可以是字符串、对象或数组")
	}
	if o["stream"] != nil {
		// 静默忽略会让按流式写的客户端一直等一个不会来的事件流。
		return errors.New("System One 不支持流式；请去掉 stream 字段")
	}
	qs := obj(o["questions"])
	if len(qs) == 0 {
		return errors.New("必须提供至少一个 questions 条目")
	}
	if len(qs) > 64 {
		return errors.New("questions 最多 64 条")
	}
	// map 遍历顺序随机，排序后报错信息才是稳定的
	keys := make([]string, 0, len(qs))
	for k := range qs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if e := validateQuestion(k, obj(qs[k])); e != nil {
			return e
		}
	}
	return nil
}

func validateQuestion(key string, q Object) error {
	if q == nil {
		return fmt.Errorf("questions.%s 必须是对象", key)
	}
	if str(q, "instructions") == "" {
		return fmt.Errorf("questions.%s 缺少 instructions", key)
	}
	switch str(q, "type") {
	case qNoul:
		// criteria 可选；给了就必须是描述 true / false 两种情形的对象
		if q["criteria"] != nil && obj(q["criteria"]) == nil {
			return fmt.Errorf("questions.%s 的 criteria 必须是对象", key)
		}
	case qChoice:
		c := obj(q["criteria"])
		if len(c) < 2 {
			return fmt.Errorf("questions.%s 是 choice，criteria 至少要有 2 个选项", key)
		}
	case qScore:
		c := arr(q["criteria"])
		if len(c) < 2 {
			return fmt.Errorf("questions.%s 是 score，criteria 至少要有 2 个分级", key)
		}
	case "":
		return fmt.Errorf("questions.%s 缺少 type", key)
	default:
		return fmt.Errorf("questions.%s 的 type 必须是 noul / choice / score", key)
	}
	return nil
}

// demoSystemOne 为 mock 供应商生成一份形状正确的演示答案，
// 让调试台和冒烟测试不依赖真实凭证。概率是算出来的占位值，不是模型判断。
func demoSystemOne(o Object, upstream string) Object {
	qs := obj(o["questions"])
	keys := make([]string, 0, len(qs))
	for k := range qs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	answers := Object{}
	for _, k := range keys {
		q := obj(qs[k])
		switch str(q, "type") {
		case qNoul:
			answers[k] = Object{"type": qNoul, "noul": 0.5}
		case qChoice:
			opts := make([]string, 0, len(obj(q["criteria"])))
			for name := range obj(q["criteria"]) {
				opts = append(opts, name)
			}
			sort.Strings(opts)
			probs := Object{}
			for _, name := range opts {
				probs[name] = roundProb(1 / float64(len(opts)))
			}
			answers[k] = Object{"type": qChoice, "choice": opts[0], "probabilities": probs, "confidence": 0.5}
		case qScore:
			levels := arr(q["criteria"])
			probs := Object{}
			for i := range levels {
				probs[fmt.Sprint(i+1)] = roundProb(1 / float64(len(levels)))
			}
			mid := (len(levels) + 1) / 2
			answers[k] = Object{"type": qScore, "score": mid, "legend": levels, "probabilities": probs, "confidence": 0.5}
		}
	}
	return Object{
		"model":   upstream,
		"answers": answers,
		"usage":   Object{"input_tokens": estimateInput(o), "output_tokens": int64(len(keys) * 12)},
	}
}

func roundProb(v float64) float64 { return math.Round(v*1e4) / 1e4 }
