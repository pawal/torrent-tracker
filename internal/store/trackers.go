package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ErrNotFound is returned when a named tracker does not exist.
var ErrNotFound = errors.New("tracker not found")

const trackerColumns = `id, name, source, enabled, created_at, last_status, last_checked_at`

func scanTracker(sc interface{ Scan(...any) error }) (Tracker, error) {
	var (
		t         Tracker
		created   string
		lastCheck sql.NullString
	)
	if err := sc.Scan(&t.ID, &t.Name, &t.Source, &t.Enabled, &created, &t.LastStatus, &lastCheck); err != nil {
		return t, err
	}
	var err error
	if t.CreatedAt, err = parseTime(created); err != nil {
		return t, err
	}
	if t.LastCheckedAt, err = parseNullTime(lastCheck); err != nil {
		return t, err
	}
	return t, nil
}

// AddTracker inserts a tracker name, reporting whether it was newly created.
// Re-adding an existing but disabled tracker re-enables it, preserving history.
func (s *Store) AddTracker(ctx context.Context, name, source string, now time.Time) (Tracker, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Tracker{}, false, err
	}
	defer tx.Rollback()

	var (
		id      int64
		enabled bool
		created bool
	)
	err = tx.QueryRowContext(ctx, `SELECT id, enabled FROM trackers WHERE name = ?`, name).Scan(&id, &enabled)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		res, err := tx.ExecContext(ctx,
			`INSERT INTO trackers (name, source, enabled, created_at) VALUES (?, ?, 1, ?)`,
			name, source, fmtTime(now))
		if err != nil {
			return Tracker{}, false, fmt.Errorf("insert tracker %s: %w", name, err)
		}
		id, _ = res.LastInsertId()
		created = true
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO changes (tracker_id, observed_at, change_type, detail) VALUES (?, ?, ?, ?)`,
			id, fmtTime(now), ChangeTrackerAdded, source); err != nil {
			return Tracker{}, false, err
		}
	case err != nil:
		return Tracker{}, false, err
	case !enabled:
		// Bring a previously removed tracker back without losing its history.
		if _, err := tx.ExecContext(ctx, `UPDATE trackers SET enabled = 1 WHERE id = ?`, id); err != nil {
			return Tracker{}, false, err
		}
	}

	t, err := scanTracker(tx.QueryRowContext(ctx, `SELECT `+trackerColumns+` FROM trackers WHERE id = ?`, id))
	if err != nil {
		return Tracker{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return Tracker{}, false, err
	}
	return t, created, nil
}

// RemoveTracker disables a tracker, keeping its history. With purge it and all
// its history are deleted outright.
func (s *Store) RemoveTracker(ctx context.Context, name string, purge bool) error {
	q := `UPDATE trackers SET enabled = 0 WHERE name = ?`
	if purge {
		q = `DELETE FROM trackers WHERE name = ?`
	}
	res, err := s.db.ExecContext(ctx, q, name)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%q: %w", name, ErrNotFound)
	}
	return nil
}

// ListTrackers returns trackers ordered by name.
func (s *Store) ListTrackers(ctx context.Context, includeDisabled bool) ([]Tracker, error) {
	q := `SELECT ` + trackerColumns + ` FROM trackers`
	if !includeDisabled {
		q += ` WHERE enabled = 1`
	}
	q += ` ORDER BY name`

	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	trackers := []Tracker{}
	for rows.Next() {
		t, err := scanTracker(rows)
		if err != nil {
			return nil, err
		}
		trackers = append(trackers, t)
	}
	return trackers, rows.Err()
}

// TrackerView is a tracker plus its currently active addresses, for list views.
type TrackerView struct {
	Tracker
	IPv4     []string     `json:"ipv4"`
	IPv6     []string     `json:"ipv6"`
	Networks []NetworkRef `json:"networks"`
}

// ListTrackerViews returns trackers with their active addresses attached.
func (s *Store) ListTrackerViews(ctx context.Context, includeDisabled bool) ([]TrackerView, error) {
	trackers, err := s.ListTrackers(ctx, includeDisabled)
	if err != nil {
		return nil, err
	}
	views := make([]TrackerView, 0, len(trackers))
	index := make(map[int64]int, len(trackers))
	for i, t := range trackers {
		views = append(views, TrackerView{
			Tracker: t, IPv4: []string{}, IPv6: []string{}, Networks: []NetworkRef{},
		})
		index[t.ID] = i
	}

	// One pass over the active addresses beats a query per tracker.
	rows, err := s.db.QueryContext(ctx, `
		SELECT tracker_id, ip, family FROM ip_records WHERE active = 1 ORDER BY family, ip`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			id     int64
			ip     string
			family int
		)
		if err := rows.Scan(&id, &ip, &family); err != nil {
			return nil, err
		}
		i, ok := index[id]
		if !ok {
			continue // disabled tracker excluded from this view
		}
		if family == 6 {
			views[i].IPv6 = append(views[i].IPv6, ip)
		} else {
			views[i].IPv4 = append(views[i].IPv4, ip)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	nets, err := s.TrackerNetworks(ctx)
	if err != nil {
		return nil, err
	}
	for id, refs := range nets {
		if i, ok := index[id]; ok {
			views[i].Networks = refs
		}
	}
	return views, nil
}

// TrackerByName looks up a single tracker.
func (s *Store) TrackerByName(ctx context.Context, name string) (Tracker, error) {
	t, err := scanTracker(s.db.QueryRowContext(ctx,
		`SELECT `+trackerColumns+` FROM trackers WHERE name = ?`, name))
	if errors.Is(err, sql.ErrNoRows) {
		return Tracker{}, fmt.Errorf("%q: %w", name, ErrNotFound)
	}
	return t, err
}
