package collector

import (
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/pawal/torrent-tracker/internal/store"
)

// testStore opens an empty database that goes away with the test.
func testStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// discard keeps the passes quiet: what they logged is never the assertion.
func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// addTracker registers a tracker with no addresses yet.
func addTracker(t *testing.T, st *store.Store, name string) store.Tracker {
	t.Helper()
	tr, _, err := st.AddTracker(t.Context(), name, "test", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	return tr
}

// resolvesTo gives a tracker its addresses the way a collection pass would, so
// a probing or enrichment test can start from a name that already resolves.
func resolvesTo(t *testing.T, st *store.Store, trackerID int64, at time.Time, ips ...string) {
	t.Helper()
	actions := make([]store.Action, 0, len(ips))
	for _, ip := range ips {
		actions = append(actions, store.Action{
			IP: ip, Family: store.Family(ip), Kind: store.ActionAdd,
		})
	}
	err := st.ApplyPlan(t.Context(), trackerID, store.Plan{
		Status: store.StatusOK, Actions: actions,
	}, at)
	if err != nil {
		t.Fatal(err)
	}
}

// changeKinds keeps the entries of a tracker's feed that match want, newest
// first. Everything else is noise to a test asking about one transition.
func changeKinds(t *testing.T, st *store.Store, trackerID int64, want ...string) []string {
	t.Helper()
	changes, err := st.ChangesFor(t.Context(), trackerID, 200)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, c := range changes {
		for _, w := range want {
			if c.Type == w {
				out = append(out, c.Type)
			}
		}
	}
	return out
}

// countKind counts one kind of entry in a tracker's feed.
func countKind(t *testing.T, st *store.Store, trackerID int64, kind string) int {
	t.Helper()
	return len(changeKinds(t, st, trackerID, kind))
}

// enrichedInto records that an address sits in a prefix, which is what rolling
// detection collapses a churning family onto.
func enrichedInto(t *testing.T, st *store.Store, ip, prefix string) {
	t.Helper()
	err := st.PutIPInfo(t.Context(), store.IPInfo{
		IP: ip, Family: store.Family(ip), ASN: 16509, Prefix: prefix,
	}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
}
