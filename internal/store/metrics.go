package store

import (
	"database/sql"
	"time"
)

// MetricMinute is one persisted dashboard sample: peak rates within the
// minute plus the on-disk byte count at its end. Minute resolution keeps the
// table small (~130k rows for the full 90-day retention).
type MetricMinute struct {
	TS     int64 // unix, minute start
	Req    float64
	Lat    float64
	Bps    float64
	Stored int64
}

// metricsRetention bounds the history kept for the status dashboard.
// ponytail: fixed 90 days; make it config if anyone ever asks.
const metricsRetention = 90 * 24 * time.Hour

// AddMetricMinute persists one rollup and prunes expired rows while it holds
// the writer anyway (usually deletes zero or one row).
func (db *DB) AddMetricMinute(m MetricMinute) error {
	return db.write(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`DELETE FROM metrics_minutes WHERE ts < ?`,
			time.Now().Add(-metricsRetention).Unix()); err != nil {
			return err
		}
		_, err := tx.Exec(`INSERT OR REPLACE INTO metrics_minutes (ts, req, lat, bps, stored) VALUES (?,?,?,?,?)`,
			m.TS, m.Req, m.Lat, m.Bps, m.Stored)
		return err
	})
}

// MetricRange returns rollups with from <= ts < to, ascending.
func (db *DB) MetricRange(from, to int64) ([]MetricMinute, error) {
	rows, err := db.r.Query(`SELECT ts, req, lat, bps, stored FROM metrics_minutes WHERE ts >= ? AND ts < ? ORDER BY ts`, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MetricMinute
	for rows.Next() {
		var m MetricMinute
		if err := rows.Scan(&m.TS, &m.Req, &m.Lat, &m.Bps, &m.Stored); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
