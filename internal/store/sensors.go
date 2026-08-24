package store

import (
	"database/sql"
	"fmt"
	"time"
)

// SensorState 為單一感測的最新狀態快照。
type SensorState struct {
	SensorID     string
	State        string // healthy / warning / critical / …
	LastValue    float64
	LastNotifyAt time.Time // 零值表示從未通知
	UpdatedAt    time.Time
}

// SetState 寫入（upsert）感測狀態。
func (s *Store) SetState(st SensorState) error {
	if st.SensorID == "" {
		return fmt.Errorf("sensor_id 不可為空")
	}
	if st.UpdatedAt.IsZero() {
		st.UpdatedAt = time.Now().UTC()
	}
	var lastNotify any
	if !st.LastNotifyAt.IsZero() {
		lastNotify = st.LastNotifyAt.UTC().Format(time.RFC3339Nano)
	}
	_, err := s.db.Exec(`INSERT INTO sensor_state(sensor_id, state, last_value, last_notify_at, updated_at)
		VALUES(?,?,?,?,?)
		ON CONFLICT(sensor_id) DO UPDATE SET
		  state=excluded.state, last_value=excluded.last_value,
		  last_notify_at=excluded.last_notify_at, updated_at=excluded.updated_at`,
		st.SensorID, st.State, st.LastValue, lastNotify,
		st.UpdatedAt.UTC().Format(time.RFC3339Nano))
	return err
}

// GetState 回傳感測狀態；不存在時回傳 (nil, nil)。
func (s *Store) GetState(sensorID string) (*SensorState, error) {
	row := s.db.QueryRow(`SELECT state, COALESCE(last_value,0), last_notify_at, updated_at
		FROM sensor_state WHERE sensor_id = ?`, sensorID)
	var st SensorState
	var lastNotify, updatedAt sql.NullString
	st.SensorID = sensorID
	err := row.Scan(&st.State, &st.LastValue, &lastNotify, &updatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if lastNotify.Valid && lastNotify.String != "" {
		st.LastNotifyAt, _ = time.Parse(time.RFC3339Nano, lastNotify.String)
	}
	if updatedAt.Valid {
		st.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updatedAt.String)
	}
	return &st, nil
}

// Prediction 為一次 ETA/預算預測紀錄（供 /accuracy 自評）。
type Prediction struct {
	ID              int64
	SensorID        string
	PredictedAt     time.Time
	EtaAggressive   *float64 // 秒；nil 表示該視野無風險或無法預測
	EtaConservative *float64
	ActualValue     float64 // 預測當下的指標實際值
	CatalogVersion  string  // 感測目錄版本，供調整前後命中率對比
}

// AppendPrediction 記錄一次預測。
func (s *Store) AppendPrediction(p Prediction) error {
	if p.SensorID == "" {
		return fmt.Errorf("sensor_id 不可為空")
	}
	if p.PredictedAt.IsZero() {
		p.PredictedAt = time.Now().UTC()
	}
	var etaA, etaC any
	if p.EtaAggressive != nil {
		etaA = *p.EtaAggressive
	}
	if p.EtaConservative != nil {
		etaC = *p.EtaConservative
	}
	res, err := s.db.Exec(`INSERT INTO predictions
		(sensor_id, predicted_at, eta_aggressive, eta_conservative, actual_value, catalog_version)
		VALUES(?,?,?,?,?,?)`,
		p.SensorID, p.PredictedAt.UTC().Format(time.RFC3339Nano),
		etaA, etaC, p.ActualValue, p.CatalogVersion)
	if err != nil {
		return err
	}
	if p.ID == 0 {
		p.ID, _ = res.LastInsertId()
	}
	return nil
}

// ListPredictions 回傳某感測自 since 起的預測紀紀錄（舊→新）。
func (s *Store) ListPredictions(sensorID string, since time.Time) ([]Prediction, error) {
	rows, err := s.db.Query(`SELECT id, predicted_at, eta_aggressive, eta_conservative,
		actual_value, COALESCE(catalog_version,'')
		FROM predictions WHERE sensor_id = ? AND predicted_at >= ?
		ORDER BY predicted_at ASC`, sensorID, since.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Prediction
	for rows.Next() {
		var pr Prediction
		var predAt string
		var etaA, etaC sql.NullFloat64
		pr.SensorID = sensorID
		if err := rows.Scan(&pr.ID, &predAt, &etaA, &etaC, &pr.ActualValue, &pr.CatalogVersion); err != nil {
			return nil, err
		}
		pr.PredictedAt, _ = time.Parse(time.RFC3339Nano, predAt)
		if etaA.Valid {
			v := etaA.Float64
			pr.EtaAggressive = &v
		}
		if etaC.Valid {
			v := etaC.Float64
			pr.EtaConservative = &v
		}
		out = append(out, pr)
	}
	return out, rows.Err()
}
