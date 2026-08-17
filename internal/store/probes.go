package store

import (
	"context"
	"fmt"
	"time"
)

// Reach is how much of a tracker answers the protocol. A separate axis from
// Status: a parked domain resolves perfectly and answers nothing.
type Reach string

const (
	ReachLive    Reach = "live"    // every probed endpoint answers
	ReachPartial Reach = "partial" // some answer, some do not
	ReachDead    Reach = "dead"    // nothing answers
	ReachUnknown Reach = "unknown" // nothing could be probed
)

// Answers reports whether the tracker responded anywhere.
func (r Reach) Answers() bool { return r == ReachLive || r == ReachPartial }

// Known reports whether the reachability was actually measured.
func (r Reach) Known() bool { return r == ReachLive || r == ReachPartial || r == ReachDead }

// ProbeResult is the verdict for one (endpoint, address) pair.
type ProbeResult string

const (
	ProbeLive    ProbeResult = "live"
	ProbeDead    ProbeResult = "dead"
	ProbeUnknown ProbeResult = "unknown"
)

// Endpoint is one announce endpoint of a tracker.
type Endpoint struct {
	ID        int64     `json:"id"`
	TrackerID int64     `json:"tracker_id"`
	Scheme    string    `json:"scheme"`
	Port      int       `json:"port"`
	Path      string    `json:"path"`
	FirstSeen time.Time `json:"first_seen"`
}

// Label is the short form used where the hostname is already known.
func (e Endpoint) Label() string { return fmt.Sprintf("%s:%d", e.Scheme, e.Port) }

// Probe is the latest verdict for one endpoint on one address.
type Probe struct {
	EndpointID int64       `json:"endpoint_id"`
	IP         string      `json:"ip"`
	Family     int         `json:"family"`
	Result     ProbeResult `json:"result"`
	Reason     string      `json:"reason,omitempty"`
	RTTms      int         `json:"rtt_ms,omitempty"`
	MissCount  int         `json:"-"`
	Since      time.Time   `json:"since"`
	CheckedAt  time.Time   `json:"checked_at"`
}

// RollUp reduces per-address probe results to one verdict for the name.
// Unknown results abstain: they neither prove life nor prove death.
func RollUp(probes []Probe) Reach {
	var live, dead int
	for _, p := range probes {
		switch p.Result {
		case ProbeLive:
			live++
		case ProbeDead:
			dead++
		}
	}
	switch {
	case live > 0 && dead > 0:
		return ReachPartial
	case live > 0:
		return ReachLive
	case dead > 0:
		return ReachDead
	}
	return ReachUnknown
}

// ReachChange maps a transition onto a change-feed entry, or "" when it is not
// news. Moving to unknown never is: failing to probe says nothing.
func ReachChange(prev, next Reach) string {
	if prev == next || !next.Known() {
		return ""
	}
	switch {
	case next == ReachDead:
		return ChangeTrackerDown
	case next == ReachPartial && prev == ReachLive:
		return ChangeTrackerPartial
	}
	// Anything else answers more than it did: recovered, or measured for the
	// first time and found alive.
	return ChangeTrackerUp
}

// AddEndpoint records an announce endpoint for a tracker, reporting whether it
// was new.
func (s *Store) AddEndpoint(ctx context.Context, trackerID int64, scheme string, port int, path string, now time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO endpoints (tracker_id, scheme, port, path, first_seen)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (tracker_id, scheme, port, path) DO NOTHING`,
		trackerID, scheme, port, path, fmtTime(now))
	if err != nil {
		return false, fmt.Errorf("add endpoint %s:%d for tracker %d: %w", scheme, port, trackerID, err)
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

const endpointColumns = `id, tracker_id, scheme, port, path, first_seen`

func scanEndpoint(sc interface{ Scan(...any) error }) (Endpoint, error) {
	var (
		e     Endpoint
		first string
	)
	if err := sc.Scan(&e.ID, &e.TrackerID, &e.Scheme, &e.Port, &e.Path, &first); err != nil {
		return e, err
	}
	var err error
	e.FirstSeen, err = parseTime(first)
	return e, err
}

// EndpointsFor returns a tracker's announce endpoints.
func (s *Store) EndpointsFor(ctx context.Context, trackerID int64) ([]Endpoint, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+endpointColumns+` FROM endpoints WHERE tracker_id = ? ORDER BY scheme, port, path`, trackerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Endpoint{}
	for rows.Next() {
		e, err := scanEndpoint(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ProbesFor returns the latest probe results across a tracker's endpoints.
func (s *Store) ProbesFor(ctx context.Context, trackerID int64) ([]Probe, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT p.endpoint_id, p.ip, p.family, p.result, p.reason, p.rtt_ms,
		       p.miss_count, p.since, p.checked_at
		FROM probes p JOIN endpoints e ON e.id = p.endpoint_id
		WHERE e.tracker_id = ?
		ORDER BY e.scheme, e.port, p.family, p.ip`, trackerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Probe{}
	for rows.Next() {
		var (
			p              Probe
			since, checked string
		)
		if err := rows.Scan(&p.EndpointID, &p.IP, &p.Family, &p.Result, &p.Reason,
			&p.RTTms, &p.MissCount, &since, &checked); err != nil {
			return nil, err
		}
		var err error
		if p.Since, err = parseTime(since); err != nil {
			return nil, err
		}
		if p.CheckedAt, err = parseTime(checked); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// PutProbes replaces the results for the given endpoints in one transaction.
// Addresses not probed this round are dropped; probes describe the present.
func (s *Store) PutProbes(ctx context.Context, endpointIDs []int64, probes []Probe) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	keep := map[int64]map[string]bool{}
	for _, p := range probes {
		if keep[p.EndpointID] == nil {
			keep[p.EndpointID] = map[string]bool{}
		}
		keep[p.EndpointID][p.IP] = true

		if _, err := tx.ExecContext(ctx, `
			INSERT INTO probes (endpoint_id, ip, family, result, reason, rtt_ms, miss_count, since, checked_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (endpoint_id, ip) DO UPDATE SET
				result = excluded.result, reason = excluded.reason, rtt_ms = excluded.rtt_ms,
				miss_count = excluded.miss_count, since = excluded.since, checked_at = excluded.checked_at`,
			p.EndpointID, p.IP, p.Family, p.Result, p.Reason, p.RTTms, p.MissCount,
			fmtTime(p.Since), fmtTime(p.CheckedAt)); err != nil {
			return fmt.Errorf("store probe %d/%s: %w", p.EndpointID, p.IP, err)
		}
	}

	for _, id := range endpointIDs {
		rows, err := tx.QueryContext(ctx, `SELECT ip FROM probes WHERE endpoint_id = ?`, id)
		if err != nil {
			return err
		}
		var stale []string
		for rows.Next() {
			var ip string
			if err := rows.Scan(&ip); err != nil {
				rows.Close()
				return err
			}
			if !keep[id][ip] {
				stale = append(stale, ip)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, ip := range stale {
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM probes WHERE endpoint_id = ? AND ip = ?`, id, ip); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

// SetReach records a tracker's reachability, appending a change when the
// transition is news. It reports the previous value and whether an entry was
// written — not merely whether the value moved, since a first measurement
// landing on unknown moves it without being news.
func (s *Store) SetReach(ctx context.Context, trackerID int64, reach Reach, detail string, now time.Time) (Reach, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", false, err
	}
	defer tx.Rollback()

	var prev Reach
	if err := tx.QueryRowContext(ctx, `SELECT reach FROM trackers WHERE id = ?`, trackerID).Scan(&prev); err != nil {
		return "", false, err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE trackers SET reach = ?, reach_checked_at = ? WHERE id = ?`,
		reach, fmtTime(now), trackerID); err != nil {
		return prev, false, err
	}
	kind := ReachChange(prev, reach)
	if kind != "" {
		if err := insertChangeNullIP(ctx, tx, trackerID, fmtTime(now), kind, detail); err != nil {
			return prev, false, err
		}
	}
	return prev, kind != "", tx.Commit()
}

// ReachSummary counts enabled trackers per reachability state. Never-probed
// names count as unknown, so the totals add up.
func (s *Store) ReachSummary(ctx context.Context) (map[Reach]int, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT reach, COUNT(*) FROM trackers
		WHERE enabled = 1 AND control = 0 GROUP BY reach`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[Reach]int{}
	for rows.Next() {
		var (
			r Reach
			n int
		)
		if err := rows.Scan(&r, &n); err != nil {
			return nil, err
		}
		if r == "" {
			r = ReachUnknown
		}
		out[r] += n
	}
	return out, rows.Err()
}

// EndpointCoverage reports how much of the registry can be probed at all.
type EndpointCoverage struct {
	Trackers      int `json:"trackers"`
	WithEndpoints int `json:"with_endpoints"`
	Endpoints     int `json:"endpoints"`
	Probed        int `json:"probed"`
}

// ProbeCoverage separates "not probed yet" from "probed and dead", which the
// reachability totals alone cannot distinguish.
func (s *Store) ProbeCoverage(ctx context.Context) (EndpointCoverage, error) {
	var c EndpointCoverage
	err := s.db.QueryRowContext(ctx, `
		SELECT (SELECT COUNT(*) FROM trackers WHERE enabled = 1 AND control = 0),
		       (SELECT COUNT(DISTINCT e.tracker_id) FROM endpoints e
		          JOIN trackers t ON t.id = e.tracker_id
		         WHERE t.enabled = 1 AND t.control = 0),
		       (SELECT COUNT(*) FROM endpoints e
		          JOIN trackers t ON t.id = e.tracker_id
		         WHERE t.enabled = 1 AND t.control = 0),
		       (SELECT COUNT(*) FROM trackers
		         WHERE enabled = 1 AND control = 0 AND reach_checked_at IS NOT NULL)`).
		Scan(&c.Trackers, &c.WithEndpoints, &c.Endpoints, &c.Probed)
	return c, err
}

// ProbeTarget is one tracker's probing work: its endpoints and the addresses
// to try them on.
type ProbeTarget struct {
	Tracker   Tracker
	Endpoints []Endpoint
	// Addrs are the active addresses, excluding prefix records.
	Addrs []string
	// Rolling lists prefix-tracked families, whose addresses churn too fast to
	// probe individually.
	Rolling []int
}

// ProbeTargets assembles the work for every enabled tracker with an endpoint.
// Names added bare have none, so there is nothing to speak to.
func (s *Store) ProbeTargets(ctx context.Context) ([]ProbeTarget, error) {
	trackers, err := s.ListTrackers(ctx, false)
	if err != nil {
		return nil, err
	}
	index := map[int64]int{}
	targets := make([]ProbeTarget, 0, len(trackers))
	for _, t := range trackers {
		index[t.ID] = len(targets)
		targets = append(targets, ProbeTarget{Tracker: t})
	}

	eps, err := s.db.QueryContext(ctx,
		`SELECT `+endpointColumns+` FROM endpoints ORDER BY tracker_id, scheme, port, path`)
	if err != nil {
		return nil, err
	}
	defer eps.Close()
	for eps.Next() {
		e, err := scanEndpoint(eps)
		if err != nil {
			return nil, err
		}
		if i, ok := index[e.TrackerID]; ok {
			targets[i].Endpoints = append(targets[i].Endpoints, e)
		}
	}
	if err := eps.Err(); err != nil {
		return nil, err
	}

	addrs, err := s.db.QueryContext(ctx,
		`SELECT tracker_id, ip FROM ip_records WHERE active = 1 AND is_prefix = 0 ORDER BY family, ip`)
	if err != nil {
		return nil, err
	}
	defer addrs.Close()
	for addrs.Next() {
		var (
			id int64
			ip string
		)
		if err := addrs.Scan(&id, &ip); err != nil {
			return nil, err
		}
		if i, ok := index[id]; ok {
			targets[i].Addrs = append(targets[i].Addrs, ip)
		}
	}
	if err := addrs.Err(); err != nil {
		return nil, err
	}

	rolling, err := s.db.QueryContext(ctx,
		`SELECT tracker_id, family FROM family_state WHERE rolling = 1 ORDER BY family`)
	if err != nil {
		return nil, err
	}
	defer rolling.Close()
	for rolling.Next() {
		var id int64
		var family int
		if err := rolling.Scan(&id, &family); err != nil {
			return nil, err
		}
		if i, ok := index[id]; ok {
			targets[i].Rolling = append(targets[i].Rolling, family)
		}
	}
	if err := rolling.Err(); err != nil {
		return nil, err
	}

	out := make([]ProbeTarget, 0, len(targets))
	for _, t := range targets {
		if len(t.Endpoints) > 0 {
			out = append(out, t)
		}
	}
	return out, nil
}

// CountEndpoints reports how many announce endpoints are on record.
func (s *Store) CountEndpoints(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM endpoints`).Scan(&n)
	return n, err
}
