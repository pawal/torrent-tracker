package store

import (
	"context"
	"testing"
	"time"
)

// One row per pass would draw a month of hourly samples as 720 slivers, so the
// log is coalesced into one interval per stretch of unchanged status. The
// intervals must abut: a gap between them would read as "never asked", when in
// fact the status simply held from one sample to the next.
func TestResolutionHistoryCoalescesUnchangedStatus(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	tr := mustAdd(t, s, "a.example.com")

	// ok, ok, ok, servfail, ok — four hourly passes then a recovery, which is
	// three intervals, not five.
	statuses := []Status{StatusOK, StatusOK, StatusOK, StatusServFail, StatusOK}
	for i, st := range statuses {
		if err := s.ApplyPlan(ctx, tr.ID, Plan{
			Status: st, Duration: time.Duration(10+i) * time.Millisecond,
		}, base.Add(time.Duration(i)*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}

	got, stats, err := s.ResolutionHistoryFor(ctx, tr.ID, base.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d intervals, want 3: %+v", len(got), got)
	}
	if got[0].Status != StatusOK || got[0].Lookups != 3 {
		t.Errorf("first interval = %q over %d lookups, want ok over 3",
			got[0].Status, got[0].Lookups)
	}
	// The ok stretch has to end where the servfail starts, or the lane shows a
	// hole for an hour nobody was in doubt about.
	if !got[0].Until.Equal(got[1].Since) {
		t.Errorf("intervals do not abut: %s then %s", got[0].Until, got[1].Since)
	}
	if got[1].Status != StatusServFail || got[1].Lookups != 1 {
		t.Errorf("second interval = %q over %d lookups, want servfail over 1",
			got[1].Status, got[1].Lookups)
	}
	// The newest sample has nothing after it to bound it, so it is an instant
	// rather than a guess about the future.
	if !got[2].Since.Equal(got[2].Until) {
		t.Errorf("newest interval spans %s → %s, want an instant", got[2].Since, got[2].Until)
	}

	if stats.Lookups != 5 {
		t.Errorf("stats over %d lookups, want 5", stats.Lookups)
	}
	// Durations are 10..14 ms, so nearest-rank puts the median at 12 and the
	// 95th percentile at the slowest reading.
	if stats.MedianMs != 12 || stats.P95ms != 14 {
		t.Errorf("median/p95 = %d/%d ms, want 12/14", stats.MedianMs, stats.P95ms)
	}
}

// The window is what the page draws, and retention is what stops the log from
// growing for as long as the daemon runs.
func TestResolutionHistoryWindowAndPruning(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	tr := mustAdd(t, s, "a.example.com")

	old := base.AddDate(0, 0, -100)
	recent := base.AddDate(0, 0, -1)
	for _, at := range []time.Time{old, recent} {
		if err := s.ApplyPlan(ctx, tr.ID, Plan{Status: StatusOK}, at); err != nil {
			t.Fatal(err)
		}
	}

	month, _, err := s.ResolutionHistoryFor(ctx, tr.ID, base.AddDate(0, 0, -30))
	if err != nil {
		t.Fatal(err)
	}
	if len(month) != 1 || month[0].Lookups != 1 {
		t.Fatalf("month window = %+v, want the recent sample only", month)
	}

	deleted, err := s.PruneLookups(ctx, base.AddDate(0, 0, -30))
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 1 {
		t.Errorf("pruned %d rows, want 1", deleted)
	}
	left, _, err := s.ResolutionHistoryFor(ctx, tr.ID, base.AddDate(0, 0, -365))
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 1 {
		t.Errorf("%d intervals survived the prune, want 1", len(left))
	}
}
