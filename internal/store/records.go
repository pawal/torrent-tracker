package store

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"sort"
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
	// ActionSupersede closes an interval without a change entry, for a record
	// replaced when a family switches between address and prefix tracking.
	ActionSupersede ActionKind = "supersede"
)

// Action is one decision about one address, or about one prefix when Prefix is
// set and IP holds a CIDR.
type Action struct {
	IP     string
	Family int
	Kind   ActionKind
	Prefix bool
	// Silent keeps the record out of the change feed. Set on the adds that
	// rewrite a family in its new tracking mode: ips_rolling and ips_stable
	// already say what happened, and the addresses are the ones we were
	// already watching, just recorded differently.
	Silent bool
}

// FamilyState is the per-family churn bookkeeping behind rolling detection.
// Fingerprint is of the last observed address set, so churn stays measurable
// once the family is stored as prefixes.
type FamilyState struct {
	Family      int    `json:"family"`
	Fingerprint string `json:"-"`
	Churn       int    `json:"churn"`
	Steady      int    `json:"steady"`
	Rolling     bool   `json:"rolling"`

	// ModeChanged marks a family that has just started or stopped rolling.
	ModeChanged bool `json:"-"`
	// Detail describes the new mode for the change feed.
	Detail string `json:"-"`
}

// Plan is the full outcome of diffing an observation against stored state.
// It is produced by the collector as a pure value, then applied here.
type Plan struct {
	Status        Status
	PrevStatus    Status
	StatusChanged bool
	Actions       []Action
	// States is the churn bookkeeping for the families this plan touched.
	States []FamilyState
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
		if (a.Kind == ActionAdd && !a.Silent) || a.Kind == ActionRemove {
			n++
		}
	}
	for _, st := range p.States {
		if st.ModeChanged {
			n++
		}
	}
	return n
}

const recordColumns = `id, tracker_id, ip, family, first_seen, last_seen, active, miss_count, is_prefix`

// ActiveRecords returns the currently open address intervals for a tracker.
func (s *Store) ActiveRecords(ctx context.Context, trackerID int64) ([]IPRecord, error) {
	return s.queryRecords(ctx, `
		SELECT `+recordColumns+`
		FROM ip_records WHERE tracker_id = ? AND active = 1 ORDER BY family, ip`, trackerID)
}

// RecordsFor returns every address interval for a tracker, newest first.
func (s *Store) RecordsFor(ctx context.Context, trackerID int64) ([]IPRecord, error) {
	return s.queryRecords(ctx, `
		SELECT `+recordColumns+`
		FROM ip_records WHERE tracker_id = ?
		ORDER BY active DESC, first_seen DESC, ip`, trackerID)
}

// FamilyStates returns the churn bookkeeping for a tracker, keyed by family.
func (s *Store) FamilyStates(ctx context.Context, trackerID int64) (map[int]FamilyState, error) {
	out := map[int]FamilyState{}
	err := s.eachRow(ctx, `
		SELECT family, fingerprint, churn, steady, rolling
		FROM family_state WHERE tracker_id = ?`, []any{trackerID}, func(sc scanner) error {
		var st FamilyState
		if err := sc.Scan(&st.Family, &st.Fingerprint, &st.Churn, &st.Steady, &st.Rolling); err != nil {
			return err
		}
		out[st.Family] = st
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func scanRecord(sc scanner) (IPRecord, error) {
	var (
		r           IPRecord
		first, last string
	)
	if err := sc.Scan(&r.ID, &r.TrackerID, &r.IP, &r.Family, &first, &last,
		&r.Active, &r.MissCount, &r.IsPrefix); err != nil {
		return r, err
	}
	var err error
	if r.FirstSeen, err = parseTime(first); err != nil {
		return r, err
	}
	r.LastSeen, err = parseTime(last)
	return r, err
}

func (s *Store) queryRecords(ctx context.Context, q string, args ...any) ([]IPRecord, error) {
	return queryAll(ctx, s, scanRecord, q, args...)
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
				INSERT INTO ip_records (tracker_id, ip, family, first_seen, last_seen, active, miss_count, is_prefix)
				VALUES (?, ?, ?, ?, ?, 1, 0, ?)`,
				trackerID, a.IP, a.Family, ts, ts, a.Prefix); err != nil {
				return fmt.Errorf("add %s: %w", a.IP, err)
			}
			if !a.Silent {
				if err := insertChange(ctx, tx, trackerID, ts, addedType(a), a.IP, a.Family, ""); err != nil {
					return err
				}
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
		case ActionRemove, ActionSupersede:
			if _, err := tx.ExecContext(ctx, `
				UPDATE ip_records SET active = 0 WHERE tracker_id = ? AND ip = ? AND active = 1`,
				trackerID, a.IP); err != nil {
				return fmt.Errorf("%s %s: %w", a.Kind, a.IP, err)
			}
			// Superseding swaps how a family is recorded rather than losing an
			// address, so only a removal reaches the feed.
			if a.Kind == ActionRemove {
				if err := insertChange(ctx, tx, trackerID, ts, removedType(a), a.IP, a.Family, ""); err != nil {
					return err
				}
			}
		default:
			return fmt.Errorf("unknown action kind %q", a.Kind)
		}
	}

	for _, st := range plan.States {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO family_state (tracker_id, family, fingerprint, churn, steady, rolling)
			VALUES (?, ?, ?, ?, ?, ?)
			ON CONFLICT (tracker_id, family) DO UPDATE SET
				fingerprint = excluded.fingerprint, churn = excluded.churn,
				steady = excluded.steady, rolling = excluded.rolling`,
			trackerID, st.Family, st.Fingerprint, st.Churn, st.Steady, st.Rolling); err != nil {
			return fmt.Errorf("family state %d: %w", st.Family, err)
		}
		if !st.ModeChanged {
			continue
		}
		kind := ChangeIPsStable
		if st.Rolling {
			kind = ChangeIPsRolling
		}
		if err := insertChange(ctx, tx, trackerID, ts, kind, "", st.Family, st.Detail); err != nil {
			return err
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

// StatusInterval is one stretch of time a name held one DNS status, coalesced
// from the per-pass lookup log.
type StatusInterval struct {
	Status Status    `json:"status"`
	Error  string    `json:"error,omitempty"`
	Since  time.Time `json:"since"`
	Until  time.Time `json:"until"`
	// Lookups is how many passes agreed, so a thinly sampled interval shows.
	Lookups int `json:"lookups"`
}

// ResolutionStats summarises the lookups behind one window. Percentiles, not a
// mean: one resolver timeout would drag an average past every real reading.
type ResolutionStats struct {
	Lookups  int `json:"lookups"`
	MedianMs int `json:"median_ms"`
	P95ms    int `json:"p95_ms"`
}

// ResolutionHistoryFor coalesces a tracker's lookup log into one interval per
// stretch of unchanged status, oldest first, plus the window's latency. Each
// sample speaks for the time until the next one, which is all a poll can.
func (s *Store) ResolutionHistoryFor(ctx context.Context, trackerID int64, since time.Time) (
	[]StatusInterval, ResolutionStats, error,
) {
	var (
		out       = []StatusInterval{}
		durations []int
	)
	err := s.eachRow(ctx, `
		SELECT ts, status, duration_ms, error FROM lookups
		WHERE tracker_id = ? AND ts >= ?
		ORDER BY ts`, []any{trackerID, fmtTime(since)}, func(sc scanner) error {
		var (
			ts       string
			status   Status
			duration int
			lookErr  string
		)
		if err := sc.Scan(&ts, &status, &duration, &lookErr); err != nil {
			return err
		}
		at, err := parseTime(ts)
		if err != nil {
			return err
		}
		durations = append(durations, duration)

		// A sample that agrees extends the interval instead of starting one.
		if n := len(out); n > 0 && out[n-1].Status == status {
			out[n-1].Until = at
			out[n-1].Lookups++
			return nil
		}
		if n := len(out); n > 0 {
			out[n-1].Until = at
		}
		out = append(out, StatusInterval{
			Status: status, Error: lookErr, Since: at, Until: at, Lookups: 1,
		})
		return nil
	})
	if err != nil {
		return nil, ResolutionStats{}, err
	}
	return out, resolutionStats(durations), nil
}

func resolutionStats(durations []int) ResolutionStats {
	if len(durations) == 0 {
		return ResolutionStats{}
	}
	sorted := append([]int(nil), durations...)
	sort.Ints(sorted)
	return ResolutionStats{
		Lookups:  len(sorted),
		MedianMs: percentile(sorted, 0.5),
		P95ms:    percentile(sorted, 0.95),
	}
}

// percentile picks the nearest-rank value from an ascending slice.
func percentile(sorted []int, p float64) int {
	i := int(math.Ceil(p*float64(len(sorted)))) - 1
	return sorted[min(max(i, 0), len(sorted)-1)]
}

// PruneLookups drops samples older than cutoff. Written once per tracker per
// pass, so without this it is the one table that grows without bound.
func (s *Store) PruneLookups(ctx context.Context, cutoff time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM lookups WHERE ts < ?`, fmtTime(cutoff))
	if err != nil {
		return 0, fmt.Errorf("prune lookups: %w", err)
	}
	return res.RowsAffected()
}

func addedType(a Action) string {
	if a.Prefix {
		return ChangePrefixAdded
	}
	return ChangeIPAdded
}

func removedType(a Action) string {
	if a.Prefix {
		return ChangePrefixRemoved
	}
	return ChangeIPRemoved
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

func scanChange(sc scanner) (Change, error) {
	var (
		c        Change
		observed string
		ip       sql.NullString
		family   sql.NullInt64
	)
	if err := sc.Scan(&c.ID, &c.TrackerID, &c.Tracker, &observed, &c.Type, &ip, &family, &c.Detail); err != nil {
		return c, err
	}
	var err error
	if c.ObservedAt, err = parseTime(observed); err != nil {
		return c, err
	}
	c.IP = ip.String
	c.Family = int(family.Int64)
	return c, nil
}

func (s *Store) queryChanges(ctx context.Context, q string, args ...any) ([]Change, error) {
	return queryAll(ctx, s, scanChange, q, args...)
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
	return queryAll(ctx, s, scanRun, `
		SELECT id, started_at, finished_at, tracker_count, ok_count, error_count, change_count
		FROM runs ORDER BY started_at DESC, id DESC LIMIT ?`, limit)
}

func scanRun(sc scanner) (Run, error) {
	var (
		r        Run
		started  string
		finished sql.NullString
	)
	if err := sc.Scan(&r.ID, &started, &finished, &r.TrackerCount, &r.OKCount, &r.ErrorCount, &r.ChangeCount); err != nil {
		return r, err
	}
	var err error
	if r.StartedAt, err = parseTime(started); err != nil {
		return r, err
	}
	r.FinishedAt, err = parseNullTime(finished)
	return r, err
}

// SharedAddress is one address more than one tracker name resolves to. Two
// names on one host are one operator and one failure domain, however different
// the names look — but behind a CDN they are one front end and nothing more,
// which is why the network comes with it.
type SharedAddress struct {
	IP       string     `json:"ip"`
	Family   int        `json:"family"`
	Trackers []string   `json:"trackers"`
	LastSeen time.Time  `json:"last_seen"`
	Network  NetworkRef `json:"network"`
	// Active is whether at least two of the names still resolve to it, as
	// opposed to having passed through it inside the window.
	Active bool `json:"active"`
}

// SharedAddresses returns the addresses shared by more than one enabled
// tracker, most shared first. Records that ended inside the window count, so a
// host handing out a rotating subset of its addresses still matches; prefix
// records do not, since a shared /48 is a shared CDN and not a shared host.
//
// Parked names are left out. They all share their parking address by
// definition and the control-name detector already has them; a parking operator
// no control name points at still shows up here as the cluster it is.
func (s *Store) SharedAddresses(ctx context.Context, since time.Time, limit int) ([]SharedAddress, error) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	index := map[string]int{}
	active := map[string]int{}
	out := []SharedAddress{}
	err := s.eachRow(ctx, `
		SELECT r.ip, r.family, t.name, MAX(r.last_seen), MAX(r.active),
		       COALESCE(i.asn, 0),
		       COALESCE(NULLIF(i.org, ''), NULLIF(i.as_name, ''), i.network_name, ''),
		       COALESCE(i.rir, ''), COALESCE(i.country, '')
		FROM ip_records r
		JOIN trackers t ON t.id = r.tracker_id
		LEFT JOIN ip_info i ON i.ip = r.ip
		WHERE r.is_prefix = 0 AND t.enabled = 1 AND t.control = 0 AND t.parked = 0
		  AND (r.active = 1 OR r.last_seen >= ?)
		GROUP BY r.ip, t.id
		ORDER BY r.ip, t.name`, []any{fmtTime(since)}, func(sc scanner) error {
		var (
			a          SharedAddress
			name, last string
			live       int
		)
		if err := sc.Scan(&a.IP, &a.Family, &name, &last, &live,
			&a.Network.ASN, &a.Network.Holder, &a.Network.RIR, &a.Network.Country); err != nil {
			return err
		}
		seen, err := parseTime(last)
		if err != nil {
			return err
		}
		i, ok := index[a.IP]
		if !ok {
			i = len(out)
			index[a.IP] = i
			out = append(out, a)
		}
		out[i].Trackers = append(out[i].Trackers, name)
		if seen.After(out[i].LastSeen) {
			out[i].LastSeen = seen
		}
		if live == 1 {
			active[a.IP]++
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read shared addresses: %w", err)
	}

	shared := make([]SharedAddress, 0, len(out))
	for _, a := range out {
		if len(a.Trackers) < 2 {
			continue
		}
		a.Active = active[a.IP] > 1
		shared = append(shared, a)
	}
	sort.Slice(shared, func(i, j int) bool {
		if len(shared[i].Trackers) != len(shared[j].Trackers) {
			return len(shared[i].Trackers) > len(shared[j].Trackers)
		}
		return shared[i].IP < shared[j].IP
	})
	if len(shared) > limit {
		shared = shared[:limit]
	}
	return shared, nil
}
