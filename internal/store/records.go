package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// ActionKind is what a collection pass decided to do with one address.
type ActionKind string

const (
	// ActionAdd opens a new interval for an address not previously active.
	ActionAdd ActionKind = "add"
	// ActionRefresh extends the interval of an address seen again.
	ActionRefresh ActionKind = "refresh"
	// ActionMiss records that an active address was absent, without closing it
	// yet: it takes MissThreshold consecutive misses to call it gone.
	ActionMiss ActionKind = "miss"
	// ActionRemove closes an interval and emits an ip_removed change.
	ActionRemove ActionKind = "remove"
)

// Action is one decision about one address.
type Action struct {
	IP     string
	Family int
	Kind   ActionKind
}

// Plan is the full outcome of diffing an observation against stored state.
// It is produced by the collector as a pure value, then applied here.
type Plan struct {
	Status        Status
	PrevStatus    Status
	StatusChanged bool
	Actions       []Action
	// LookupErr is the resolver error, if any, for the audit trail.
	LookupErr string
	// Duration is how long the lookup took.
	Duration time.Duration
}

// Changes counts the entries this plan will append to the change feed.
func (p Plan) Changes() int {
	n := 0
	if p.StatusChanged {
		n++
	}
	for _, a := range p.Actions {
		if a.Kind == ActionAdd || a.Kind == ActionRemove {
			n++
		}
	}
	return n
}

// ActiveRecords returns the currently open address intervals for a tracker.
func (s *Store) ActiveRecords(ctx context.Context, trackerID int64) ([]IPRecord, error) {
	return s.queryRecords(ctx, `
		SELECT id, tracker_id, ip, family, first_seen, last_seen, active, miss_count
		FROM ip_records WHERE tracker_id = ? AND active = 1 ORDER BY family, ip`, trackerID)
}

// RecordsFor returns every address interval for a tracker, newest first.
func (s *Store) RecordsFor(ctx context.Context, trackerID int64) ([]IPRecord, error) {
	return s.queryRecords(ctx, `
		SELECT id, tracker_id, ip, family, first_seen, last_seen, active, miss_count
		FROM ip_records WHERE tracker_id = ?
		ORDER BY active DESC, first_seen DESC, ip`, trackerID)
}

func (s *Store) queryRecords(ctx context.Context, q string, args ...any) ([]IPRecord, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	records := []IPRecord{}
	for rows.Next() {
		var (
			r           IPRecord
			first, last string
		)
		if err := rows.Scan(&r.ID, &r.TrackerID, &r.IP, &r.Family, &first, &last, &r.Active, &r.MissCount); err != nil {
			return nil, err
		}
		if r.FirstSeen, err = parseTime(first); err != nil {
			return nil, err
		}
		if r.LastSeen, err = parseTime(last); err != nil {
			return nil, err
		}
		records = append(records, r)
	}
	return records, rows.Err()
}

// ApplyPlan persists one tracker's collection result atomically: address
// intervals, change-feed entries, the lookup audit row, and tracker status.
func (s *Store) ApplyPlan(ctx context.Context, trackerID int64, plan Plan, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	ts := fmtTime(now)

	for _, a := range plan.Actions {
		switch a.Kind {
		case ActionAdd:
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO ip_records (tracker_id, ip, family, first_seen, last_seen, active, miss_count)
				VALUES (?, ?, ?, ?, ?, 1, 0)`,
				trackerID, a.IP, a.Family, ts, ts); err != nil {
				return fmt.Errorf("add %s: %w", a.IP, err)
			}
			if err := insertChange(ctx, tx, trackerID, ts, ChangeIPAdded, a.IP, a.Family, ""); err != nil {
				return err
			}
		case ActionRefresh:
			if _, err := tx.ExecContext(ctx, `
				UPDATE ip_records SET last_seen = ?, miss_count = 0
				WHERE tracker_id = ? AND ip = ? AND active = 1`, ts, trackerID, a.IP); err != nil {
				return fmt.Errorf("refresh %s: %w", a.IP, err)
			}
		case ActionMiss:
			if _, err := tx.ExecContext(ctx, `
				UPDATE ip_records SET miss_count = miss_count + 1
				WHERE tracker_id = ? AND ip = ? AND active = 1`, trackerID, a.IP); err != nil {
				return fmt.Errorf("miss %s: %w", a.IP, err)
			}
		case ActionRemove:
			if _, err := tx.ExecContext(ctx, `
				UPDATE ip_records SET active = 0 WHERE tracker_id = ? AND ip = ? AND active = 1`,
				trackerID, a.IP); err != nil {
				return fmt.Errorf("remove %s: %w", a.IP, err)
			}
			if err := insertChange(ctx, tx, trackerID, ts, ChangeIPRemoved, a.IP, a.Family, ""); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unknown action kind %q", a.Kind)
		}
	}

	if plan.StatusChanged {
		detail := fmt.Sprintf("%s -> %s", orUnchecked(plan.PrevStatus), plan.Status)
		if err := insertChangeNullIP(ctx, tx, trackerID, ts, ChangeStatusChanged, detail); err != nil {
			return err
		}
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO lookups (tracker_id, ts, status, duration_ms, error) VALUES (?, ?, ?, ?, ?)`,
		trackerID, ts, plan.Status, plan.Duration.Milliseconds(), plan.LookupErr); err != nil {
		return err
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE trackers SET last_status = ?, last_checked_at = ? WHERE id = ?`,
		plan.Status, ts, trackerID); err != nil {
		return err
	}

	return tx.Commit()
}

func orUnchecked(s Status) string {
	if s == "" {
		return "unchecked"
	}
	return string(s)
}

func insertChange(ctx context.Context, tx *sql.Tx, trackerID int64, ts, kind, ip string, family int, detail string) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO changes (tracker_id, observed_at, change_type, ip, family, detail)
		VALUES (?, ?, ?, ?, ?, ?)`, trackerID, ts, kind, ip, family, detail)
	return err
}

func insertChangeNullIP(ctx context.Context, tx *sql.Tx, trackerID int64, ts, kind, detail string) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO changes (tracker_id, observed_at, change_type, detail)
		VALUES (?, ?, ?, ?)`, trackerID, ts, kind, detail)
	return err
}

const changeColumns = `c.id, c.tracker_id, t.name, c.observed_at, c.change_type, c.ip, c.family, c.detail`

func (s *Store) queryChanges(ctx context.Context, q string, args ...any) ([]Change, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	changes := []Change{}
	for rows.Next() {
		var (
			c        Change
			observed string
			ip       sql.NullString
			family   sql.NullInt64
		)
		if err := rows.Scan(&c.ID, &c.TrackerID, &c.Tracker, &observed, &c.Type, &ip, &family, &c.Detail); err != nil {
			return nil, err
		}
		if c.ObservedAt, err = parseTime(observed); err != nil {
			return nil, err
		}
		c.IP = ip.String
		c.Family = int(family.Int64)
		changes = append(changes, c)
	}
	return changes, rows.Err()
}

// RecentChanges returns the newest changes, optionally limited to those at or
// after since (pass the zero time for no lower bound).
func (s *Store) RecentChanges(ctx context.Context, since time.Time, limit int) ([]Change, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	q := `SELECT ` + changeColumns + ` FROM changes c JOIN trackers t ON t.id = c.tracker_id`
	args := []any{}
	if !since.IsZero() {
		q += ` WHERE c.observed_at >= ?`
		args = append(args, fmtTime(since))
	}
	q += ` ORDER BY c.observed_at DESC, c.id DESC LIMIT ?`
	args = append(args, limit)
	return s.queryChanges(ctx, q, args...)
}

// ChangesFor returns the newest changes for one tracker.
func (s *Store) ChangesFor(ctx context.Context, trackerID int64, limit int) ([]Change, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	return s.queryChanges(ctx, `
		SELECT `+changeColumns+` FROM changes c JOIN trackers t ON t.id = c.tracker_id
		WHERE c.tracker_id = ? ORDER BY c.observed_at DESC, c.id DESC LIMIT ?`, trackerID, limit)
}

// StartRun opens a collection run record.
func (s *Store) StartRun(ctx context.Context, now time.Time, trackerCount int) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO runs (started_at, tracker_count) VALUES (?, ?)`, fmtTime(now), trackerCount)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// FinishRun closes a collection run record with its tallies.
func (s *Store) FinishRun(ctx context.Context, id int64, now time.Time, ok, errCount, changes int) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE runs SET finished_at = ?, ok_count = ?, error_count = ?, change_count = ? WHERE id = ?`,
		fmtTime(now), ok, errCount, changes, id)
	return err
}

// RecentRuns returns the newest collection runs.
func (s *Store) RecentRuns(ctx context.Context, limit int) ([]Run, error) {
	if limit <= 0 || limit > 200 {
		limit = 20
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, started_at, finished_at, tracker_count, ok_count, error_count, change_count
		FROM runs ORDER BY started_at DESC, id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	runs := []Run{}
	for rows.Next() {
		var (
			r        Run
			started  string
			finished sql.NullString
		)
		if err := rows.Scan(&r.ID, &started, &finished, &r.TrackerCount, &r.OKCount, &r.ErrorCount, &r.ChangeCount); err != nil {
			return nil, err
		}
		if r.StartedAt, err = parseTime(started); err != nil {
			return nil, err
		}
		if r.FinishedAt, err = parseNullTime(finished); err != nil {
			return nil, err
		}
		runs = append(runs, r)
	}
	return runs, rows.Err()
}
