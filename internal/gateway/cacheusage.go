package gateway

import "fmt"

const cacheRollupTable = `CREATE TABLE IF NOT EXISTS cache_hourly (hour INTEGER NOT NULL, key_id TEXT NOT NULL, requested_model TEXT NOT NULL, model_id TEXT NOT NULL, provider_id TEXT NOT NULL,
	cache_known_requests INTEGER NOT NULL, cache_null_requests INTEGER NOT NULL, cache_input_tokens INTEGER NOT NULL, cache_hit_tokens INTEGER NOT NULL,
	PRIMARY KEY (hour, key_id, requested_model, model_id, provider_id)) WITHOUT ROWID`

const cacheRollupFrom = `INSERT INTO cache_hourly SELECT started_at/3600000, key_id, requested_model, model_id, provider_id,
	SUM(cache_known=1), SUM(cache_known=2), SUM(CASE WHEN cache_known=1 THEN input_tokens ELSE 0 END), SUM(CASE WHEN cache_known=1 THEN cache_tokens ELSE 0 END)
	FROM requests WHERE status!='running' AND cache_known!=0 AND %s GROUP BY 1,2,3,4,5
	ON CONFLICT (hour, key_id, requested_model, model_id, provider_id) DO UPDATE SET
	cache_known_requests=cache_known_requests+excluded.cache_known_requests, cache_null_requests=cache_null_requests+excluded.cache_null_requests,
	cache_input_tokens=cache_input_tokens+excluded.cache_input_tokens, cache_hit_tokens=cache_hit_tokens+excluded.cache_hit_tokens`

var (
	cacheRollupOne  = fmt.Sprintf(cacheRollupFrom, "id=?")
	cacheRollupFill = fmt.Sprintf(cacheRollupFrom, "started_at>=?")
)

const cacheColumns = `SUM(cache_known_requests) cache_known_requests, SUM(cache_null_requests) cache_null_requests,
	SUM(cache_input_tokens) cache_input_tokens, SUM(cache_hit_tokens) cache_hit_tokens`

func cacheRates(o Object) {
	known, nulls := num(o, "cache_known_requests"), num(o, "cache_null_requests")
	o["cache_samples"] = int64(known + nulls)
	if input := num(o, "cache_input_tokens"); input > 0 {
		o["cache_hit_rate"] = num(o, "cache_hit_tokens") / input
	} else {
		o["cache_hit_rate"] = nil
	}
	if samples := known + nulls; samples > 0 {
		o["cache_null_rate"] = nulls / samples
	} else {
		o["cache_null_rate"] = nil
	}
}

func cacheStat(r map[string]any) Object {
	o := Object{}
	for _, key := range []string{"cache_known_requests", "cache_null_requests", "cache_input_tokens", "cache_hit_tokens"} {
		o[key] = int64(num(r, key))
	}
	cacheRates(o)
	return o
}
