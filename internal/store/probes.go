package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/pawal/torrent-tracker/internal/prober"
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
	// Signature fingerprints the tracker software, Kind says what sort of
	// evidence it is, Server names the front end. HTTP only: BEP 15 carries none
	// of them.
	Signature string      `json:"signature,omitempty"`
	Kind      prober.Kind `json:"signature_kind,omitempty"`
	Server    string      `json:"server,omitempty"`
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

func scanEndpoint(sc scanner) (Endpoint, error) {
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
	return queryAll(ctx, s, scanEndpoint,
		`SELECT `+endpointColumns+` FROM endpoints WHERE tracker_id = ? ORDER BY scheme, port, path`, trackerID)
}

func scanProbe(sc scanner) (Probe, error) {
	var (
		p              Probe
		since, checked string
	)
	if err := sc.Scan(&p.EndpointID, &p.IP, &p.Family, &p.Result, &p.Reason,
		&p.RTTms, &p.MissCount, &since, &checked, &p.Signature, &p.Kind,
		&p.Server); err != nil {
		return p, err
	}
	var err error
	if p.Since, err = parseTime(since); err != nil {
		return p, err
	}
	p.CheckedAt, err = parseTime(checked)
	return p, err
}

// ProbesFor returns the latest probe results across a tracker's endpoints.
func (s *Store) ProbesFor(ctx context.Context, trackerID int64) ([]Probe, error) {
	return queryAll(ctx, s, scanProbe, `
		SELECT p.endpoint_id, p.ip, p.family, p.result, p.reason, p.rtt_ms,
		       p.miss_count, p.since, p.checked_at, p.signature, p.signature_kind, p.server
		FROM probes p JOIN endpoints e ON e.id = p.endpoint_id
		WHERE e.tracker_id = ?
		ORDER BY e.scheme, e.port, p.family, p.ip`, trackerID)
}

// PutProbes replaces the results for the given endpoints in one transaction.
// Addresses not probed this round are dropped; probes describe the present. A
// replaced verdict is archived first, so only the open interval is overwritten.
// now closes the intervals of addresses that stopped being probed at all.
func (s *Store) PutProbes(ctx context.Context, endpointIDs []int64, probes []Probe, now time.Time) error {
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

		// merge holds Since still while a verdict stands, so a moved Since
		// means the previous interval ended.
		if err := archiveProbe(ctx, tx, p.EndpointID, p.IP, p.Since); err != nil {
			return err
		}

		if _, err := tx.ExecContext(ctx, `
			INSERT INTO probes (endpoint_id, ip, family, result, reason, rtt_ms, miss_count,
			                    since, checked_at, signature, signature_kind, server)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (endpoint_id, ip) DO UPDATE SET
				result = excluded.result, reason = excluded.reason, rtt_ms = excluded.rtt_ms,
				miss_count = excluded.miss_count, since = excluded.since, checked_at = excluded.checked_at,
				signature = excluded.signature, signature_kind = excluded.signature_kind,
				server = excluded.server`,
			p.EndpointID, p.IP, p.Family, p.Result, p.Reason, p.RTTms, p.MissCount,
			fmtTime(p.Since), fmtTime(p.CheckedAt), p.Signature, p.Kind, p.Server); err != nil {
			return fmt.Errorf("store probe %d/%s: %w", p.EndpointID, p.IP, err)
		}
	}

	for _, id := range endpointIDs {
		stale, err := staleAddresses(ctx, tx, id, keep[id])
		if err != nil {
			return err
		}
		for _, ip := range stale {
			if err := archiveProbe(ctx, tx, id, ip, now); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM probes WHERE endpoint_id = ? AND ip = ?`, id, ip); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

// staleAddresses lists the addresses stored for an endpoint that this round did
// not probe. Their intervals are closed rather than left claiming the present.
func staleAddresses(ctx context.Context, tx *sql.Tx, endpointID int64, keep map[string]bool) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT ip FROM probes WHERE endpoint_id = ?`, endpointID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var stale []string
	for rows.Next() {
		var ip string
		if err := rows.Scan(&ip); err != nil {
			return nil, err
		}
		if !keep[ip] {
			stale = append(stale, ip)
		}
	}
	return stale, rows.Err()
}

// ProbeInterval is one stretch of time an address held one verdict on one
// endpoint. Closed intervals only: the open one is still in probes.
type ProbeInterval struct {
	EndpointID int64       `json:"endpoint_id"`
	IP         string      `json:"ip"`
	Family     int         `json:"family"`
	Result     ProbeResult `json:"result"`
	Reason     string      `json:"reason,omitempty"`
	Since      time.Time   `json:"since"`
	Until      time.Time   `json:"until"`
}

// archiveProbe copies the stored interval for one address into probe_history.
// A verdict that has not moved, or a first probe, has nothing to close.
func archiveProbe(ctx context.Context, tx *sql.Tx, endpointID int64, ip string, until time.Time) error {
	var (
		family         int
		result, reason string
		since          string
	)
	err := tx.QueryRowContext(ctx, `
		SELECT family, result, reason, since FROM probes
		WHERE endpoint_id = ? AND ip = ?`, endpointID, ip).Scan(&family, &result, &reason, &since)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read probe %d/%s: %w", endpointID, ip, err)
	}
	started, err := parseTime(since)
	if err != nil {
		return err
	}
	if !started.Before(until) {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO probe_history (endpoint_id, ip, family, result, reason, since, until)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		endpointID, ip, family, result, reason, since, fmtTime(until)); err != nil {
		return fmt.Errorf("archive probe %d/%s: %w", endpointID, ip, err)
	}
	return nil
}

func scanProbeInterval(sc scanner) (ProbeInterval, error) {
	var (
		iv           ProbeInterval
		start, until string
	)
	if err := sc.Scan(&iv.EndpointID, &iv.IP, &iv.Family, &iv.Result, &iv.Reason,
		&start, &until); err != nil {
		return iv, err
	}
	var err error
	if iv.Since, err = parseTime(start); err != nil {
		return iv, err
	}
	iv.Until, err = parseTime(until)
	return iv, err
}

// ProbeHistoryFor returns the closed probe intervals overlapping the window
// starting at since, oldest first. Open intervals live in probes.
func (s *Store) ProbeHistoryFor(ctx context.Context, trackerID int64, since time.Time) ([]ProbeInterval, error) {
	return queryAll(ctx, s, scanProbeInterval, `
		SELECT h.endpoint_id, h.ip, h.family, h.result, h.reason, h.since, h.until
		FROM probe_history h JOIN endpoints e ON e.id = h.endpoint_id
		WHERE e.tracker_id = ? AND h.until >= ?
		ORDER BY h.endpoint_id, h.ip, h.id`, trackerID, fmtTime(since))
}

// PruneProbeHistory drops intervals that ended before cutoff, so the table is
// bounded by the retention window rather than by uptime.
func (s *Store) PruneProbeHistory(ctx context.Context, cutoff time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM probe_history WHERE until < ?`, fmtTime(cutoff))
	if err != nil {
		return 0, fmt.Errorf("prune probe history: %w", err)
	}
	return res.RowsAffected()
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
	out := map[Reach]int{}
	err := s.eachRow(ctx, `
		SELECT reach, COUNT(*) FROM trackers
		WHERE enabled = 1 AND control = 0 GROUP BY reach`, nil, func(sc scanner) error {
		var (
			r Reach
			n int
		)
		if err := sc.Scan(&r, &n); err != nil {
			return err
		}
		if r == "" {
			r = ReachUnknown
		}
		out[r] += n
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// EndpointCoverage reports how much of the registry can be probed at all.
type EndpointCoverage struct {
	Trackers      int `json:"trackers"`
	WithEndpoints int `json:"with_endpoints"`
	Endpoints     int `json:"endpoints"`
	Probed        int `json:"probed"`
	// Identified is how many trackers gave up a software signature. Far fewer
	// than Probed: UDP discloses nothing, and a dead endpoint says nothing.
	Identified int `json:"identified"`
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
		         WHERE enabled = 1 AND control = 0 AND reach_checked_at IS NOT NULL),
		       (SELECT COUNT(DISTINCT e.tracker_id) FROM probes p
		          JOIN endpoints e ON e.id = p.endpoint_id
		          JOIN trackers t ON t.id = e.tracker_id
		         WHERE t.enabled = 1 AND t.control = 0 AND p.signature != '')`).
		Scan(&c.Trackers, &c.WithEndpoints, &c.Endpoints, &c.Probed, &c.Identified)
	return c, err
}

// SoftwareStat is one cluster of trackers that answered alike.
type SoftwareStat struct {
	Signature string      `json:"signature"`
	Kind      prober.Kind `json:"kind,omitempty"`
	Trackers  int         `json:"trackers"`
	Endpoints int         `json:"endpoints"`
	// Variants are the raw signatures folded into the cluster, present only
	// where grouping dropped keys to get there.
	Variants []string `json:"variants,omitempty"`
}

// conditional keys come and go with the peers a tracker has to report or the
// extensions it chose to mention, not with the software. Grouping on them split
// one implementation across ten rows.
var conditional = map[string]bool{
	"peers":           true,
	"peers6":          true,
	"downloaded":      true,
	"min interval":    true,
	"warning message": true,
	"external ip":     true,
	"tracker id":      true,
}

// groupSignature reduces a reply shape to the keys that identify it. A failure
// text is a literal from the implementation and passes through untouched.
func groupSignature(sig string, kind prober.Kind) string {
	if kind != prober.KindShape {
		return sig
	}
	keep := make([]string, 0, 4)
	for _, k := range strings.Split(sig, ",") {
		if !conditional[k] {
			keep = append(keep, k)
		}
	}
	// Nothing but conditional keys leaves the raw shape as the only handle.
	if len(keep) == 0 {
		return sig
	}
	return strings.Join(keep, ",")
}

// SoftwareStats groups the registry by tracker software, most common first.
// Folding happens here rather than in SQL because the grouping key is not the
// stored signature: shapes lose their conditional keys first.
func (s *Store) SoftwareStats(ctx context.Context, limit int) ([]SoftwareStat, error) {
	if limit <= 0 || limit > 200 {
		limit = 25
	}
	type cluster struct {
		SoftwareStat
		trackers  map[int64]bool
		endpoints map[int64]bool
		variants  map[string]bool
	}
	groups := map[string]*cluster{}
	err := s.eachRow(ctx, `
		SELECT DISTINCT p.signature, p.signature_kind, e.tracker_id, p.endpoint_id
		FROM probes p
		JOIN endpoints e ON e.id = p.endpoint_id
		JOIN trackers t ON t.id = e.tracker_id
		WHERE t.enabled = 1 AND t.control = 0 AND p.signature != ''`, nil, func(sc scanner) error {
		var (
			sig, kind             string
			trackerID, endpointID int64
		)
		if err := sc.Scan(&sig, &kind, &trackerID, &endpointID); err != nil {
			return err
		}
		grouped := groupSignature(sig, prober.Kind(kind))
		key := kind + "\x00" + grouped
		c := groups[key]
		if c == nil {
			c = &cluster{
				SoftwareStat: SoftwareStat{Signature: grouped, Kind: prober.Kind(kind)},
				trackers:     map[int64]bool{},
				endpoints:    map[int64]bool{},
				variants:     map[string]bool{},
			}
			groups[key] = c
		}
		c.trackers[trackerID] = true
		c.endpoints[endpointID] = true
		if sig != grouped {
			c.variants[sig] = true
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	out := make([]SoftwareStat, 0, len(groups))
	for _, c := range groups {
		st := c.SoftwareStat
		st.Trackers, st.Endpoints = len(c.trackers), len(c.endpoints)
		for v := range c.variants {
			st.Variants = append(st.Variants, v)
		}
		sort.Strings(st.Variants)
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Trackers != out[j].Trackers {
			return out[i].Trackers > out[j].Trackers
		}
		return out[i].Signature < out[j].Signature
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
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

// ClearProbes closes every probe interval of a tracker, for when probing stops
// rather than measures something dead. The history keeps what was measured; the
// present stops claiming to be.
func (s *Store) ClearProbes(ctx context.Context, trackerID int64, now time.Time) error {
	eps, err := s.EndpointsFor(ctx, trackerID)
	if err != nil {
		return err
	}
	ids := make([]int64, 0, len(eps))
	for _, e := range eps {
		ids = append(ids, e.ID)
	}
	if len(ids) == 0 {
		return nil
	}
	return s.PutProbes(ctx, ids, nil, now)
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

	err = s.eachRow(ctx,
		`SELECT `+endpointColumns+` FROM endpoints ORDER BY tracker_id, scheme, port, path`,
		nil, func(sc scanner) error {
			e, err := scanEndpoint(sc)
			if err != nil {
				return err
			}
			if i, ok := index[e.TrackerID]; ok {
				targets[i].Endpoints = append(targets[i].Endpoints, e)
			}
			return nil
		})
	if err != nil {
		return nil, err
	}

	err = s.eachRow(ctx,
		`SELECT tracker_id, ip FROM ip_records WHERE active = 1 AND is_prefix = 0 ORDER BY family, ip`,
		nil, func(sc scanner) error {
			var (
				id int64
				ip string
			)
			if err := sc.Scan(&id, &ip); err != nil {
				return err
			}
			if i, ok := index[id]; ok {
				targets[i].Addrs = append(targets[i].Addrs, ip)
			}
			return nil
		})
	if err != nil {
		return nil, err
	}

	err = s.eachRollingFamily(ctx, func(id int64, family int) {
		if i, ok := index[id]; ok {
			targets[i].Rolling = append(targets[i].Rolling, family)
		}
	})
	if err != nil {
		return nil, err
	}

	out := make([]ProbeTarget, 0, len(targets))
	for _, t := range targets {
		// A host publishing BEP 34 with no endpoint has said it runs no
		// tracker. Probing it anyway would be ignoring the one opt-out the
		// protocol gives an operator.
		if len(t.Endpoints) > 0 && !t.Tracker.BEP34Denies {
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
