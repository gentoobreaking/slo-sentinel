// Package store 提供感測狀態與預測紀錄的持久化（SQLite，WAL 模式）。
//
// T004：schema 以 migrations 版本機制管理；所有寫入經單一 writer 連線序列化，
// 讀取可併發。UI（T016）不直接開啟此檔——一律走 sentinel 唯讀 API（spec.md §2.5）。
// 驅動：modernc.org/sqlite（純 Go，免 CGO）。
package store

import (
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// Store 封裝 SQLite 連線與遷移。
type Store struct {
	db *sql.DB
}

// Open 開啟（必要時建立）位於 path 的資料庫並執行遷移。
func Open(path string) (*Store, error) {
	// busy_timeout：WAL 模式下避免寫入衝突立即報錯；foreign_keys 依契約開啟
	// modernc.org/sqlite DSN pragma：WAL 日誌模式 + busy timeout（毫秒）+ 外鍵開啟
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %s: %w", path, err)
	}
	// modernc/sqlite 建議限制單一寫入連線；讀取仍可透過多連線，但簡單起見固定 1 寫
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

var migrations = []string{
	`CREATE TABLE IF NOT EXISTS schema_migrations (
		version INTEGER PRIMARY KEY,
		applied_at TEXT NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS sensor_state (
		sensor_id      TEXT PRIMARY KEY,
		state          TEXT NOT NULL,
		last_value     REAL,
		last_notify_at TEXT,
		updated_at     TEXT NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS predictions (
		id              INTEGER PRIMARY KEY AUTOINCREMENT,
		sensor_id       TEXT NOT NULL,
		predicted_at    TEXT NOT NULL,
		eta_aggressive  REAL,
		eta_conservative REAL,
		actual_value    REAL,
		catalog_version TEXT,
		created_at      TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
	)`,
	`CREATE INDEX IF NOT EXISTS idx_predictions_sensor ON predictions(sensor_id, predicted_at)`,
	// v3：容量感測的利用率（0–1）與原始消耗量分開存——budget-status 端點
	// 需要利用率算 remaining_budget%，而 last_value 是絕對量（如 bytes）
	`ALTER TABLE sensor_state ADD COLUMN last_utilization REAL`,
	// v4（T024）：waste 候選生命週期持久化——重啟不丟 dismiss/resolve 狀態
	`CREATE TABLE IF NOT EXISTS waste_entries (
		sensor_id         TEXT NOT NULL,
		resource_id       TEXT NOT NULL,
		reason            TEXT NOT NULL DEFAULT '',
		state             TEXT NOT NULL,
		first_seen        TEXT NOT NULL,
		last_notified     TEXT NOT NULL DEFAULT '',
		renotify_sec      REAL NOT NULL DEFAULT 0,
		waste_usd_per_day REAL NOT NULL DEFAULT 0,
		total_waste_usd   REAL NOT NULL DEFAULT 0,
		dismiss_reason    TEXT NOT NULL DEFAULT '',
		dismiss_until     TEXT NOT NULL DEFAULT '',
		PRIMARY KEY (sensor_id, resource_id)
	)`,
	// v5（T032）：predictions 補存天花板與使用率——歷史列為 NULL
	`ALTER TABLE predictions ADD COLUMN ceiling REAL`,
	`ALTER TABLE predictions ADD COLUMN utilization REAL`,
}

func (s *Store) migrate() error {
	if _, err := s.db.Exec(migrations[0]); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}
	for i, m := range migrations {
		if i == 0 {
			continue // 第 0 條是遷移表本身
		}
		var done int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version = ?`, i).Scan(&done); err != nil {
			return err
		}
		if done > 0 {
			continue
		}
		if _, err := s.db.Exec(m); err != nil {
			return fmt.Errorf("migration %d: %w", i, err)
		}
		if _, err := s.db.Exec(`INSERT INTO schema_migrations(version, applied_at) VALUES (?, ?)`,
			i, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			return err
		}
	}
	return nil
}

// StateRow 為 status 表格的一列。
type StateRow struct {
	SensorID  string
	State     string
	LastValue float64
	UpdatedAt time.Time
}

// AllStates 回傳全部感測狀態（依 sensor_id 排序）。
func (s *Store) AllStates() ([]StateRow, error) {
	rows, err := s.db.Query(`SELECT sensor_id, state, COALESCE(last_value,0), updated_at
		FROM sensor_state ORDER BY sensor_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StateRow
	for rows.Next() {
		var r StateRow
		var updated string
		if err := rows.Scan(&r.SensorID, &r.State, &r.LastValue, &updated); err != nil {
			return nil, err
		}
		r.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
		out = append(out, r)
	}
	return out, rows.Err()
}

// PrunePredictions 刪除 predicted_at 早於 cutoff 的預測紀錄（T029 retention）。
// 回傳刪除列數。之後執行 PRAGMA optimize 收整統計；WAL checkpoint 交由
// SQLite 自動管理（wal_autocheckpoint 預設開啟）。
func (s *Store) PrunePredictions(cutoff time.Time) (int64, error) {
	res, err := s.db.Exec(`DELETE FROM predictions WHERE predicted_at < ?`,
		cutoff.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	if _, err := s.db.Exec(`PRAGMA optimize`); err != nil {
		return n, nil // optimize 失敗不影響清理結果
	}
	return n, nil
}

// CountPredictions 回傳預測紀錄總列數（retention 測試與觀測用）。
func (s *Store) CountPredictions() (int64, error) {
	var n int64
	err := s.db.QueryRow(`SELECT COUNT(*) FROM predictions`).Scan(&n)
	return n, err
}
