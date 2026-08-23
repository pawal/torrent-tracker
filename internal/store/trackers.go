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

const trackerColumns = `id, name, source, enabled, created_at, last_status, last_checked_at,
	control, parked, reach, reach_checked_at, bep34, bep34_denies, resolve_fails, failing_since`

func scanTracker(sc scanner) (Tracker, error) {
	var (
		t          Tracker
		created    string
		lastCheck  sql.NullString
		reachCheck sql.NullString
		failing    sql.NullString
	)
	if err := sc.Scan(&t.ID, &t.Name, &t.Source, &t.Enabled, &created, &t.LastStatus, &lastCheck,
		&t.Control, &t.Parked, &t.Reach, &reachCheck, &t.BEP34, &t.BEP34Denies,
		&t.ResolveFails, &failing); err != nil {
		return t, err
	}
	var err error
	if t.CreatedAt, err = parseTime(created); err != nil {
		return t, err
	}
	if t.LastCheckedAt, err = parseNullTime(lastCheck); err != nil {
		return t, err
	}
	if t.ReachCheckedAt, err = parseNullTime(reachCheck); err != nil {
		return t, err
	}
	if t.FailingSince, err = parseNullTime(failing); err != nil {
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
		// The failing streak restarts, so a re-added name gets its full grace
		// again instead of being retired on the next pass.
		if _, err := tx.ExecContext(ctx,
			`UPDATE trackers SET enabled = 1, resolve_fails = 0, failing_since = NULL WHERE id = ?`,
			id); err != nil {
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
	return s.execOne(ctx, name, q, name)
}

// Retirement is one name dropped from collection, with the evidence for it.
type Retirement struct {
	ID         int64
	Name       string
	LastStatus Status
	Since      time.Time
}

func (r Retirement) detail(now time.Time) string {
	return fmt.Sprintf("never resolved in %d days of trying; %s",
		int(now.Sub(r.Since).Hours()/24), orUnchecked(r.LastStatus))
}

// RetireStale disables the names that have never once resolved and have been
// failing since before cutoff, keeping their history. A name that resolved even
// once is left alone: it went quiet, which is a different fact.
func (s *Store) RetireStale(ctx context.Context, cutoff, now time.Time) ([]Retirement, error) {
	stale, err := queryAll(ctx, s, func(sc scanner) (Retirement, error) {
		var (
			r     Retirement
			since string
		)
		if err := sc.Scan(&r.ID, &r.Name, &r.LastStatus, &since); err != nil {
			return r, err
		}
		var err error
		r.Since, err = parseTime(since)
		return r, err
	}, `
		SELECT id, name, last_status, failing_since FROM trackers
		WHERE enabled = 1 AND control = 0
		  AND failing_since IS NOT NULL AND failing_since <= ?
		  AND NOT EXISTS (SELECT 1 FROM ip_records r WHERE r.tracker_id = trackers.id)
		ORDER BY name`, fmtTime(cutoff))
	if err != nil || len(stale) == 0 {
		return nil, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	ts := fmtTime(now)
	for _, r := range stale {
		if _, err := tx.ExecContext(ctx, `UPDATE trackers SET enabled = 0 WHERE id = ?`, r.ID); err != nil {
			return nil, fmt.Errorf("retire %s: %w", r.Name, err)
		}
		if err := insertChangeNullIP(ctx, tx, r.ID, ts, ChangeTrackerRetired, r.detail(now)); err != nil {
			return nil, err
		}
	}
	return stale, tx.Commit()
}

// ListTrackers returns trackers ordered by name, excluding control names.
func (s *Store) ListTrackers(ctx context.Context, includeDisabled bool) ([]Tracker, error) {
	q := `SELECT ` + trackerColumns + ` FROM trackers WHERE control = 0`
	if !includeDisabled {
		q += ` AND enabled = 1`
	}
	q += ` ORDER BY name`
	return s.queryTrackers(ctx, q)
}

// ListControls returns the enabled control names.
func (s *Store) ListControls(ctx context.Context) ([]Tracker, error) {
	return s.queryTrackers(ctx,
		`SELECT `+trackerColumns+` FROM trackers WHERE control = 1 AND enabled = 1 ORDER BY name`)
}

// ListParked returns the enabled trackers currently flagged as parked.
func (s *Store) ListParked(ctx context.Context) ([]Tracker, error) {
	return s.queryTrackers(ctx,
		`SELECT `+trackerColumns+` FROM trackers WHERE parked = 1 AND enabled = 1 AND control = 0 ORDER BY name`)
}

func (s *Store) queryTrackers(ctx context.Context, q string, args ...any) ([]Tracker, error) {
	return queryAll(ctx, s, scanTracker, q, args...)
}

// SetControl marks or unmarks a name as a control.
func (s *Store) SetControl(ctx context.Context, name string, control bool) error {
	return s.execOne(ctx, name,
		`UPDATE trackers SET control = ? WHERE name = ?`, control, name)
}

// SetParked records whether a tracker resolves only to parking addresses,
// and reports whether the flag changed.
func (s *Store) SetParked(ctx context.Context, trackerID int64, parked bool, detail string, now time.Time) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	var was bool
	if err := tx.QueryRowContext(ctx, `SELECT parked FROM trackers WHERE id = ?`, trackerID).Scan(&was); err != nil {
		return false, err
	}
	if was == parked {
		return false, nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE trackers SET parked = ? WHERE id = ?`, parked, trackerID); err != nil {
		return false, err
	}
	// Only the transition into parked is news.
	if parked {
		if err := insertChangeNullIP(ctx, tx, trackerID, fmtTime(now), ChangeParked, detail); err != nil {
			return false, err
		}
	}
	return true, tx.Commit()
}

// SetBEP34 records the tracker preferences a name publishes, reporting whether
// they moved. Withdrawing a record is as much news as publishing one.
func (s *Store) SetBEP34(ctx context.Context, trackerID int64, record string, denies bool, now time.Time) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	var was string
	if err := tx.QueryRowContext(ctx, `SELECT bep34 FROM trackers WHERE id = ?`, trackerID).Scan(&was); err != nil {
		return false, err
	}
	if was == record {
		return false, nil
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE trackers SET bep34 = ?, bep34_denies = ? WHERE id = ?`,
		record, denies, trackerID); err != nil {
		return false, err
	}

	kind, detail := ChangeBEP34Changed, was+" -> "+record
	switch {
	case was == "":
		kind, detail = ChangeBEP34Added, record
	case record == "":
		kind, detail = ChangeBEP34Removed, was
	}
	if err := insertChangeNullIP(ctx, tx, trackerID, fmtTime(now), kind, detail); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// TrackerView is a tracker plus its currently active addresses, for list views.
type TrackerView struct {
	Tracker
	IPv4     []string     `json:"ipv4"`
	IPv6     []string     `json:"ipv6"`
	Networks []NetworkRef `json:"networks"`
	// Rolling lists families tracked by prefix; their IPv4/IPv6 entries are
	// CIDRs, not addresses.
	Rolling []int `json:"rolling,omitempty"`
	// Uptime is the share of measured time the tracker answered over the
	// window, null when nothing was measured.
	Uptime *float64 `json:"uptime"`
	// Misses is the failed attempts behind that uptime: 100% with misses is a
	// flapping name the intervals alone cannot show.
	Misses int `json:"misses,omitempty"`
	// State is how long the present reachability has held, null when nothing is
	// being measured now.
	State *TrackerState `json:"state"`
}

// TrackerState is the phrase a list wants rather than a colour: answering or
// silent, and since when.
type TrackerState struct {
	Answering bool      `json:"answering"`
	Since     time.Time `json:"since"`
	// Clipped marks a stretch running back to the edge of the window, so it
	// began at least that long ago rather than exactly then.
	Clipped bool `json:"clipped,omitempty"`
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
	err = s.eachRow(ctx, `
		SELECT tracker_id, ip, family FROM ip_records WHERE active = 1 ORDER BY family, ip`,
		nil, func(sc scanner) error {
			var (
				id     int64
				ip     string
				family int
			)
			if err := sc.Scan(&id, &ip, &family); err != nil {
				return err
			}
			i, ok := index[id]
			if !ok {
				return nil // disabled tracker excluded from this view
			}
			if family == 6 {
				views[i].IPv6 = append(views[i].IPv6, ip)
			} else {
				views[i].IPv4 = append(views[i].IPv4, ip)
			}
			return nil
		})
	if err != nil {
		return nil, err
	}

	err = s.eachRollingFamily(ctx, func(id int64, family int) {
		if i, ok := index[id]; ok {
			views[i].Rolling = append(views[i].Rolling, family)
		}
	})
	if err != nil {
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
