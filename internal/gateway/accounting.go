package gateway

import (
	"encoding/json"
	"math"
	"net/url"
)

func isOpenRouter(p Provider) bool {
	if p.Kind == "openrouter" {
		return true
	}
	u, err := url.Parse(p.BaseURL)
	return err == nil && u.Hostname() == "openrouter.ai"
}

func cost(m Model, u Usage) int64 {
	if !m.PricingSet {
		return 0
	}
	uncached := max(int64(0), u.Input-u.Cache-u.Write)
	v := (float64(uncached)*m.InputPrice + float64(u.Output)*m.OutputPrice + float64(u.Cache)*m.CachePrice + float64(u.Write)*m.WritePrice) * 1000
	return int64(math.Ceil(v))
}

func estimateInput(body Object) int64 { return int64((len(raw(body))+2)/3 + 16) }

func cacheUsage(o Object, keys ...string) (int64, bool, bool) {
	var total int64
	found := false
	for _, key := range keys {
		v, ok := o[key]
		if !ok {
			continue
		}
		found = true
		if v == nil {
			return 0, false, true
		}
		n, ok := numericValue(v)
		if !ok || n < 0 || math.IsNaN(n) || math.IsInf(n, 0) || n >= float64(math.MaxInt64) {
			return 0, false, false
		}
		total += int64(n)
	}
	return total, found, false
}

func numericValue(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	default:
		return 0, false
	}
}

func reportedCostNano(v any) (int64, bool) {
	n, ok := v.(float64)
	if !ok || n < 0 || math.IsNaN(n) || math.IsInf(n, 0) || n >= float64(math.MaxInt64)/1e9 {
		return 0, false
	}
	return int64(math.Ceil(n * 1e9)), true
}
