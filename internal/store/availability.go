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
	// Since is when the present stretch began: of answering when Answering, of
	// silence otherwise. Zero when nothing is being measured now, since a state
	// nobody is watching is not a present state.
	Since     time.Time
	Answering bool
	// Clipped marks a stretch running back to the start of the window, which
	// began at least that long ago rather than exactly then.
	Clipped bool
	// Misses is the failed attempts the intervals in the window survived. A
	// share of 1.0 with misses is a flapping name, not a healthy one.
	Misses int
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
type spans struct {
	measured, live []interval
	misses         int
}

func (sp *spans) add(iv interval, live bool, misses int) {
	sp.measured = append(sp.measured, iv)
	sp.misses += misses
	if live {
		sp.live = append(sp.live, iv)
	}
}

func (sp *spans) availability(window interval) Availability {
	live, measured := merge(sp.live), merge(sp.measured)
	a := Availability{Live: cover(live), Measured: cover(measured), Misses: sp.misses}
	if len(measured) == 0 {
		return a
	}
	// Nothing has been measured up to the end of the window, so there is no
	// present state to date: probing stopped, or the addresses went away.
	if measured[len(measured)-1].end.Before(window.end) {
		return a
	}
	switch {
	case len(live) > 0 && !live[len(live)-1].end.Before(window.end):
		a.Answering, a.Since = true, live[len(live)-1].start
	case len(live) > 0:
		a.Since = live[len(live)-1].end
	default:
		// Measured and never once answering, so silent since the first thing
		// anyone measured.
		a.Since = measured[0].start
	}
	a.Clipped = !a.Since.After(window.start)
	return a
}

// merge sorts intervals and folds the overlapping ones together, so a stretch
// two addresses covered at once counts as the one stretch it was.
func merge(ivs []interval) []interval {
	if len(ivs) == 0 {
		return nil
	}
	sort.Slice(ivs, func(i, j int) bool { return ivs[i].start.Before(ivs[j].start) })
	out := []interval{ivs[0]}
	for _, iv := range ivs[1:] {
		last := &out[len(out)-1]
		if iv.start.After(last.end) {
			out = append(out, iv)
			continue
		}
		if iv.end.After(last.end) {
			last.end = iv.end
		}
	}
	return out
}

// cover is the time a set of merged intervals spans in total.
func cover(merged []interval) time.Duration {
	var total time.Duration
	for _, iv := range merged {
		total += iv.end.Sub(iv.start)
	}
	return total
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

	window := interval{start: from, end: until}
	trackers, endpoints := map[int64]*spans{}, map[int64]*spans{}

	// probe_history holds the closed intervals and probes the open one, so the
	// two together cover the axis. Unknown verdicts stay out entirely.
	err := s.eachRow(ctx, `
		SELECT e.tracker_id, h.endpoint_id, h.result, h.misses, h.since, h.until
		FROM probe_history h
		JOIN endpoints e ON e.id = h.endpoint_id
		JOIN trackers t ON t.id = e.tracker_id
		WHERE t.enabled = 1 AND t.control = 0 AND h.result != ? AND h.until > ?
		UNION ALL
		SELECT e.tracker_id, p.endpoint_id, p.result, p.misses, p.since, ?
		FROM probes p
		JOIN endpoints e ON e.id = p.endpoint_id
		JOIN trackers t ON t.id = e.tracker_id
		WHERE t.enabled = 1 AND t.control = 0 AND p.result != ?`,
		[]any{ProbeUnknown, fmtTime(from), fmtTime(until), ProbeUnknown}, func(sc scanner) error {
			var (
				trackerID, endpointID int64
				result                ProbeResult
				misses                int
				since, ends           string
			)
			if err := sc.Scan(&trackerID, &endpointID, &result, &misses, &since, &ends); err != nil {
				return err
			}
			start, err := parseTime(since)
			if err != nil {
				return err
			}
			end, err := parseTime(ends)
			if err != nil {
				return err
			}
			iv, ok := interval{start: start, end: end}.clip(window)
			if !ok {
				return nil
			}
			// Misses count whole, even where the window clipped the interval:
			// what is being reported is that the stretch was not clean.
			live := result == ProbeLive
			spansFor(trackers, trackerID).add(iv, live, misses)
			spansFor(endpoints, endpointID).add(iv, live, misses)
			return nil
		})
	if err != nil {
		return w, fmt.Errorf("read probe intervals: %w", err)
	}

	for id, sp := range trackers {
		w.Trackers[id] = sp.availability(window)
	}
	for id, sp := range endpoints {
		w.Endpoints[id] = sp.availability(window)
	}
	return w, nil
}
