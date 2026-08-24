package store

// waste.go（T024）：waste 候選清單（Tracker entries）的持久化。
// daemon 每輪掃描後寫回；重啟時 AllWasteEntries 還原 dismiss/resolve 狀態。

import (
	"database/sql"
	"fmt"
	"time"
)

// WasteEntry 對應 waste.Tracker 的一個候選追蹤紀蹤紀錄（欄位語意同 waste.Entry）。
type WasteEntry struct {
	SensorID       string
	ResourceID     string
	Reason         string
	State          string // notified | renoted | dismissed | resolved
	FirstSeen      time.Time
	LastNotified   time.Time
	Renotify       time.Duration // 0 = 只提一次
	WasteUSDPerDay float64
	TotalWasteUSD  float64
	DismissReason  string
	DismissUntil   time.Time // 零值 = 不復活
}

// SetWasteEntry 寫入（upsert）一筆候選紀錄。
func (s *Store) SetWasteEntry(e WasteEntry) error {
	if e.SensorID == "" || e.ResourceID == "" {
		return fmt.Errorf("sensor_id 與 resource_id 不可為空")
	}
	firstSeen := e.FirstSeen
	if firstSeen.IsZero() {
		firstSeen = time.Now().UTC()
	}
	var renotify any
	if e.Renotify > 0 {
		renotify = e.Renotify.Seconds()
	}
	var dismissUntil string
	if !e.DismissUntil.IsZero() {
		dismissUntil = e.DismissUntil.UTC().Format(time.RFC3339Nano)
	}
	_, err := s.db.Exec(`INSERT INTO waste_entries
		(sensor_id, resource_id, reason, state, first_seen, last_notified,
		 renotify_sec, waste_usd_per_day, total_waste_usd, dismiss_reason, dismiss_until)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(sensor_id, resource_id) DO UPDATE SET
		  reason=excluded.reason, state=excluded.state, first_seen=excluded.first_seen,
		  last_notified=excluded.last_notified, renotify_sec=excluded.renotify_sec,
		  waste_usd_per_day=excluded.waste_usd_per_day, total_waste_usd=excluded.total_waste_usd,
		  dismiss_reason=excluded.dismiss_reason, dismiss_until=excluded.dismiss_until`,
		e.SensorID, e.ResourceID, e.Reason, e.State,
		firstSeen.UTC().Format(time.RFC3339Nano), e.LastNotified.UTC().Format(time.RFC3339Nano),
		renotify, e.WasteUSDPerDay, e.TotalWasteUSD, e.DismissReason, dismissUntil)
	return err
}

// AllWasteEntries 回傳全部候選紀錄（依 sensor_id、resource_id 排序）。
func (s *Store) AllWasteEntries() ([]WasteEntry, error) {
	rows, err := s.db.Query(`SELECT sensor_id, resource_id, reason, state,
		first_seen, last_notified, COALESCE(renotify_sec,0),
		COALESCE(waste_usd_per_day,0), COALESCE(total_waste_usd,0),
		dismiss_reason, dismiss_until
		FROM waste_entries ORDER BY sensor_id, resource_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WasteEntry
	for rows.Next() {
		var e WasteEntry
		var firstSeen, lastNotified, dismissUntil string
		var renotify float64
		if err := rows.Scan(&e.SensorID, &e.ResourceID, &e.Reason, &e.State,
			&firstSeen, &lastNotified, &renotify,
			&e.WasteUSDPerDay, &e.TotalWasteUSD, &e.DismissReason, &dismissUntil); err != nil {
			return nil, err
		}
		e.FirstSeen, _ = time.Parse(time.RFC3339Nano, firstSeen)
		e.LastNotified, _ = time.Parse(time.RFC3339Nano, lastNotified)
		if dismissUntil != "" {
			e.DismissUntil, _ = time.Parse(time.RFC3339Nano, dismissUntil)
		}
		e.Renotify = time.Duration(renotify) * time.Second
		out = append(out, e)
	}
	return out, rows.Err()
}

// ResolvedWasteSaving 回傳已結案（resolved）候選的累積節省金額總和。
func (s *Store) ResolvedWasteSaving() (float64, error) {
	row := s.db.QueryRow(`SELECT COALESCE(SUM(total_waste_usd),0)
		FROM waste_entries WHERE state = 'resolved'`)
	var v float64
	err := row.Scan(&v)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return v, err
}
