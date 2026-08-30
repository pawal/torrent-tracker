package store

import (
	"testing"
	"time"
)

// The dataset's temporalCoverage starts here, so it has to be the oldest name
// and not merely some name.
func TestEarliestTracker(t *testing.T) {
	s := testStore(t)
	ctx := t.Context()

	// An empty database has no coverage to declare, and must not invent one.
	got, err := s.EarliestTracker(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !got.IsZero() {
		t.Errorf("empty store = %s, want the zero time", got)
	}

	if _, _, err := s.AddTracker(ctx, "later.example.com", "test", base.Add(48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.AddTracker(ctx, "first.example.com", "test", base); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.AddTracker(ctx, "middle.example.com", "test", base.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	got, err = s.EarliestTracker(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Equal(base) {
		t.Errorf("EarliestTracker = %s, want %s", got, base)
	}
}
