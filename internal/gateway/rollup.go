package gateway

// 用量小时汇总：请求结束时把这条请求累加进 usage_hourly，统计界面只读这张小表，
// 不必每次扫描不断增长的请求明细。累加直接取刚写完的请求行，两边口径天然一致。
// 口径与 model.stats 相同：只算已结束的请求，演示请求不计花费，未计价指成功但价格未确认的请求。

import (
	"fmt"
	"log/slog"

	"prism-gateway/internal/sqlite"
)

const rollupTable = `CREATE TABLE IF NOT EXISTS usage_hourly (hour INTEGER NOT NULL, key_id TEXT NOT NULL, requested_model TEXT NOT NULL, model_id TEXT NOT NULL, provider_id TEXT NOT NULL,
	requests INTEGER NOT NULL, success INTEGER NOT NULL, errors INTEGER NOT NULL, input_tokens INTEGER NOT NULL, output_tokens INTEGER NOT NULL, cache_tokens INTEGER NOT NULL,
	cost_nano INTEGER NOT NULL, unpriced INTEGER NOT NULL, success_ms INTEGER NOT NULL, last_at INTEGER NOT NULL,
	PRIMARY KEY (hour, key_id, requested_model, model_id, provider_id)) WITHOUT ROWID`

// rollupFrom 把满足条件的已结束请求按小时汇总后合并进表；%s 处填 WHERE 条件
const rollupFrom = `INSERT INTO usage_hourly SELECT started_at/3600000, key_id, requested_model, model_id, provider_id,
	COUNT(*), SUM(status='success'), SUM(status IN ('error','unknown')), SUM(input_tokens), SUM(output_tokens), SUM(cache_tokens),
	SUM(CASE WHEN is_demo=0 THEN cost_nano ELSE 0 END), SUM(status='success' AND cost_known=0 AND is_demo=0),
	SUM(CASE WHEN status='success' THEN duration_ms ELSE 0 END), MAX(started_at)
	FROM requests WHERE status!='running' AND %s GROUP BY 1,2,3,4,5
	ON CONFLICT (hour, key_id, requested_model, model_id, provider_id) DO UPDATE SET
	requests=requests+excluded.requests, success=success+excluded.success, errors=errors+excluded.errors,
	input_tokens=input_tokens+excluded.input_tokens, output_tokens=output_tokens+excluded.output_tokens, cache_tokens=cache_tokens+excluded.cache_tokens,
	cost_nano=cost_nano+excluded.cost_nano, unpriced=unpriced+excluded.unpriced, success_ms=success_ms+excluded.success_ms,
	last_at=MAX(last_at, excluded.last_at)`

var (
	rollupOne  = fmt.Sprintf(rollupFrom, "id=?")
	rollupFill = fmt.Sprintf(rollupFrom, "started_at>=?")
)

// rollup 把刚结束的一条请求累加进汇总表。失败只影响统计，不影响记账，记日志即可
func (e *Engine) rollup(id string) {
	if err := e.store.DB.Exec(rollupOne, id); err != nil {
		slog.Error("usage rollup write failed", "request_id", id, "err", err)
	}
}

// rebuildRollup 从 fromMS 所在小时起用请求明细重建汇总，计价补录这类批量改写之后调用
func rebuildRollup(t *sqlite.Tx, fromMS int64) error {
	hour := fromMS / 3600000
	if err := t.Exec("DELETE FROM usage_hourly WHERE hour>=?", hour); err != nil {
		return err
	}
	return t.Exec(rollupFill, hour*3600000)
}
