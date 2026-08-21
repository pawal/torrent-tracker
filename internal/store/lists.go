package store

import (
	"context"
	"fmt"
	"sort"
	"time"
)

// ListEntry is one announce endpoint offered to a client, with the uptime that
// decides which lists it belongs on.
type ListEntry struct {
	TrackerID int64
	Tracker   string
	Scheme    string
	URL       string
	Uptime    Availability
	Added     time.Time
	// Answers is whether this endpoint is answering now, which is not the same
	// as the name answering: a tracker can be live on udp:6969 and dead on
	// https:443.
	Answers bool
	// V4 and V6 are the families the name currently resolves to, so a list can
	// leave out what the caller cannot reach.
	V4 bool
	V6 bool
}

// ListEndpoints assembles the announce endpoints eligible for a client list,
// best uptime first. Disabled, control and parked names never appear: a parked
// domain resolves perfectly and answers nothing.
func (s *Store) ListEndpoints(ctx context.Context, from, until time.Time) ([]ListEntry, error) {
	win, err := s.AvailabilityOver(ctx, from, until)
	if err != nil {
		return nil, err
	}

	live, err := s.answeringEndpoints(ctx)
	if err != nil {
		return nil, err
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT t.id, t.name, t.created_at, e.id, e.scheme, e.port, e.path
		FROM endpoints e
		JOIN trackers t ON t.id = e.tracker_id
		WHERE t.enabled = 1 AND t.control = 0 AND t.parked = 0`)
	if err != nil {
		return nil, fmt.Errorf("read list endpoints: %w", err)
	}
	defer rows.Close()

	out := []ListEntry{}
	for rows.Next() {
		var (
			e            ListEntry
			created      string
			endpointID   int64
			scheme, path string
			port         int
		)
		if err := rows.Scan(&e.TrackerID, &e.Tracker, &created,
			&endpointID, &scheme, &port, &path); err != nil {
			return nil, err
		}
		if e.Added, err = parseTime(created); err != nil {
			return nil, err
		}
		e.Scheme = scheme
		e.URL = fmt.Sprintf("%s://%s:%d%s", scheme, e.Tracker, port, path)
		e.Uptime = win.Endpoints[endpointID]
		e.Answers = live[endpointID]
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// A prefix record still says the family resolves, so rolling names count.
	fams, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT tracker_id, family FROM ip_records WHERE active = 1`)
	if err != nil {
		return nil, err
	}
	defer fams.Close()
	v4, v6 := map[int64]bool{}, map[int64]bool{}
	for fams.Next() {
		var (
			id     int64
			family int
		)
		if err := fams.Scan(&id, &family); err != nil {
			return nil, err
		}
		if family == 6 {
			v6[id] = true
		} else {
			v4[id] = true
		}
	}
	if err := fams.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		out[i].V4, out[i].V6 = v4[out[i].TrackerID], v6[out[i].TrackerID]
	}

	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if x, y := a.Uptime.Share(), b.Uptime.Share(); x != y {
			return x > y
		}
		if a.Tracker != b.Tracker {
			return a.Tracker < b.Tracker
		}
		return a.URL < b.URL
	})
	return out, nil
}

// answeringEndpoints is the set of endpoints with at least one address
// answering now. probes describes the present, so one live row is enough.
func (s *Store) answeringEndpoints(ctx context.Context) (map[int64]bool, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT endpoint_id FROM probes WHERE result = ?`, ProbeLive)
	if err != nil {
		return nil, fmt.Errorf("read answering endpoints: %w", err)
	}
	defer rows.Close()

	out := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}
