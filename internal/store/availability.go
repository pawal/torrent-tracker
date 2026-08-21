package store

import (
	"context"
	"fmt"
	"sort"
	"time"
)

// Availability is how much of a window something answered. Measured time is the
// denominator, so an unprobed stretch neither helps nor hurts: the same
// abstention rule the reachability rollup uses.
type Availability struct {
	Live     time.Duration
	Measured time.Duration
}

// Share is the fraction of measured time spent answering.
func (a Availability) Share() float64 {
	if a.Measured <= 0 {
		return 0
	}
	return float64(a.Live) / float64(a.Measured)
}

// Known reports whether anything was measured at all.
func (a Availability) Known() bool { return a.Measured > 0 }

// AvailabilityWindow is uptime per tracker and per endpoint over one window.
type AvailabilityWindow struct {
	From      time.Time
	Until     time.Time
	Trackers  map[int64]Availability
	Endpoints map[int64]Availability
}

// interval is one stretch of wall-clock time.
type interval struct{ start, end time.Time }

// clip trims an interval to the window, reporting whether anything is left.
func (iv interval) clip(w interval) (interval, bool) {
	if iv.start.Before(w.start) {
		iv.start = w.start
	}
	if iv.end.After(w.end) {
		iv.end = w.end
	}
	return iv, iv.end.After(iv.start)
}

// spans collects the intervals behind one uptime figure, overlaps included.
type spans struct{ measured, live []interval }

func (sp *spans) add(iv interval, live bool) {
	sp.measured = append(sp.measured, iv)
	if live {
		sp.live = append(sp.live, iv)
	}
}

func (sp *spans) availability() Availability {
	return Availability{Live: cover(sp.live), Measured: cover(sp.measured)}
}

// cover is the time a set of intervals spans in total, counting overlap once.
func cover(ivs []interval) time.Duration {
	if len(ivs) == 0 {
		return 0
	}
	sort.Slice(ivs, func(i, j int) bool { return ivs[i].start.Before(ivs[j].start) })
	var total time.Duration
	cur := ivs[0]
	for _, iv := range ivs[1:] {
		if iv.start.After(cur.end) {
			total += cur.end.Sub(cur.start)
			cur = iv
			continue
		}
		if iv.end.After(cur.end) {
			cur.end = iv.end
		}
	}
	return total + cur.end.Sub(cur.start)
}

func spansFor(m map[int64]*spans, id int64) *spans {
	sp := m[id]
	if sp == nil {
		sp = &spans{}
		m[id] = sp
	}
	return sp
}

// AvailabilityOver reports uptime over the window for every enabled tracker and
// its endpoints. A name's lanes are unioned rather than averaged: a client needs
// one (endpoint, address) pair to work, not all of them, so the time counts as
// up whenever anything answered.
func (s *Store) AvailabilityOver(ctx context.Context, from, until time.Time) (AvailabilityWindow, error) {
	w := AvailabilityWindow{
		From: from, Until: until,
		Trackers:  map[int64]Availability{},
		Endpoints: map[int64]Availability{},
	}
	if !from.Before(until) {
		return w, nil
	}

	// probe_history holds the closed intervals and probes the open one, so the
	// two together cover the axis. Unknown verdicts stay out entirely.
	rows, err := s.db.QueryContext(ctx, `
		SELECT e.tracker_id, h.endpoint_id, h.result, h.since, h.until
		FROM probe_history h
		JOIN endpoints e ON e.id = h.endpoint_id
		JOIN trackers t ON t.id = e.tracker_id
		WHERE t.enabled = 1 AND t.control = 0 AND h.result != ? AND h.until > ?
		UNION ALL
		SELECT e.tracker_id, p.endpoint_id, p.result, p.since, ?
		FROM probes p
		JOIN endpoints e ON e.id = p.endpoint_id
		JOIN trackers t ON t.id = e.tracker_id
		WHERE t.enabled = 1 AND t.control = 0 AND p.result != ?`,
		ProbeUnknown, fmtTime(from), fmtTime(until), ProbeUnknown)
	if err != nil {
		return w, fmt.Errorf("read probe intervals: %w", err)
	}
	defer rows.Close()

	window := interval{start: from, end: until}
	trackers, endpoints := map[int64]*spans{}, map[int64]*spans{}
	for rows.Next() {
		var (
			trackerID, endpointID int64
			result                ProbeResult
			since, ends           string
		)
		if err := rows.Scan(&trackerID, &endpointID, &result, &since, &ends); err != nil {
			return w, err
		}
		start, err := parseTime(since)
		if err != nil {
			return w, err
		}
		end, err := parseTime(ends)
		if err != nil {
			return w, err
		}
		iv, ok := interval{start: start, end: end}.clip(window)
		if !ok {
			continue
		}
		live := result == ProbeLive
		spansFor(trackers, trackerID).add(iv, live)
		spansFor(endpoints, endpointID).add(iv, live)
	}
	if err := rows.Err(); err != nil {
		return w, err
	}

	for id, sp := range trackers {
		w.Trackers[id] = sp.availability()
	}
	for id, sp := range endpoints {
		w.Endpoints[id] = sp.availability()
	}
	return w, nil
}
