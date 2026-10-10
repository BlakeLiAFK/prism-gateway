package gateway

import (
	"encoding/json"

	"prism-gateway/internal/sqlite"
)

func (s *Store) loadSettings() (Settings, error) {
	settings := defaults().Settings
	rows, err := s.DB.Query("SELECT key,value FROM settings UNION ALL SELECT key,value FROM meta WHERE key='request_diagnostics_until'")
	if err != nil {
		return settings, err
	}
	m := Object{}
	json.Unmarshal([]byte(raw(settings)), &m)
	for _, row := range rows {
		var v any
		if err := json.Unmarshal([]byte(row.String("value")), &v); err != nil {
			return settings, err
		}
		m[row.String("key")] = v
	}
	err = decode(m, &settings)
	return settings, err
}

func saveSettings(tx *sqlite.Tx, settings Settings) error {
	var values Object
	json.Unmarshal([]byte(raw(settings)), &values)
	for k, v := range values {
		table := "settings"
		// 旧版本严格校验 settings 的键，临时诊断状态单独保存以允许直接回滚。
		if k == "request_diagnostics_until" {
			table = "meta"
		}
		if err := tx.Exec("INSERT INTO "+table+" VALUES (?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", k, raw(v)); err != nil {
			return err
		}
	}
	return nil
}
