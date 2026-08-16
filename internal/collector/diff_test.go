package collector

import (
	"testing"

	"github.com/pawal/torrent-tracker/internal/resolver"
	"github.com/pawal/torrent-tracker/internal/store"
)

func ok(addrs ...string) resolver.Result {
	return resolver.Result{Status: store.StatusOK, Addrs: addrs}
}

func fail(s store.Status) resolver.Result { return resolver.Result{Status: s, Err: string(s)} }

func nodata() resolver.Result { return resolver.Result{Status: store.StatusNoData} }

func nxdomain() resolver.Result { return resolver.Result{Status: store.StatusNXDomain} }

func active(ip string, family, missCount int) store.IPRecord {
	return store.IPRecord{IP: ip, Family: family, Active: true, MissCount: missCount}
}

// actionSet renders a plan's actions as a comparable set of "kind ip" strings.
func actionSet(p store.Plan) map[string]bool {
	set := map[string]bool{}
	for _, a := range p.Actions {
		set[string(a.Kind)+" "+a.IP] = true
	}
	return set
}

func wantActions(t *testing.T, p store.Plan, want ...string) {
	t.Helper()
	got := actionSet(p)
	if len(got) != len(want) {
		t.Errorf("action count = %d, want %d\ngot:  %v\nwant: %v", len(got), len(want), keys(got), want)
		return
	}
	for _, w := range want {
		if !got[w] {
			t.Errorf("missing action %q\ngot: %v", w, keys(got))
		}
	}
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestObservationStatus(t *testing.T) {
	tests := []struct {
		name    string
		a, aaaa resolver.Result
		want    store.Status
	}{
		{"both ok", ok("1.2.3.4"), ok("2001:db8::1"), store.StatusOK},
		{"v4 only", ok("1.2.3.4"), nodata(), store.StatusOK},
		{"v6 only", nodata(), ok("2001:db8::1"), store.StatusOK},
		{"no addresses at all", nodata(), nodata(), store.StatusNoData},
		{"name gone", nxdomain(), nxdomain(), store.StatusNXDomain},
		// NXDOMAIN is a fact about the name, so it outranks a transport failure.
		{"nxdomain beats timeout", nxdomain(), fail(store.StatusTimeout), store.StatusNXDomain},
		// A failure means we do not know, which is worse news than a known empty.
		{"failure beats nodata", nodata(), fail(store.StatusServFail), store.StatusServFail},
		{"servfail outranks timeout", fail(store.StatusTimeout), fail(store.StatusServFail), store.StatusServFail},
		{"an answer beats a failure", ok("1.2.3.4"), fail(store.StatusServFail), store.StatusOK},
		{"both dead", fail(store.StatusTimeout), fail(store.StatusTimeout), store.StatusTimeout},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Observation{A: tt.a, AAAA: tt.aaaa}.Status()
			if got != tt.want {
				t.Errorf("Status() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDiffFirstObservation(t *testing.T) {
	plan := Diff(nil, "", Observation{A: ok("1.2.3.4", "5.6.7.8"), AAAA: ok("2001:db8::1")}, 1)

	if plan.Status != store.StatusOK {
		t.Errorf("status = %q, want ok", plan.Status)
	}
	if !plan.StatusChanged {
		t.Error("first observation should count as a status change")
	}
	wantActions(t, plan, "add 1.2.3.4", "add 5.6.7.8", "add 2001:db8::1")
	if got := plan.Changes(); got != 4 { // 3 additions + 1 status change
		t.Errorf("Changes() = %d, want 4", got)
	}
}

func TestDiffSteadyState(t *testing.T) {
	prev := []store.IPRecord{active("1.2.3.4", 4, 0), active("2001:db8::1", 6, 0)}
	plan := Diff(prev, store.StatusOK, Observation{A: ok("1.2.3.4"), AAAA: ok("2001:db8::1")}, 1)

	if plan.StatusChanged {
		t.Error("unchanged status should not be reported as a change")
	}
	wantActions(t, plan, "refresh 1.2.3.4", "refresh 2001:db8::1")
	if got := plan.Changes(); got != 0 {
		t.Errorf("Changes() = %d, want 0 for a steady state", got)
	}
}

func TestDiffAddressReplaced(t *testing.T) {
	prev := []store.IPRecord{active("1.2.3.4", 4, 0)}
	plan := Diff(prev, store.StatusOK, Observation{A: ok("9.9.9.9"), AAAA: nodata()}, 1)

	wantActions(t, plan, "add 9.9.9.9", "remove 1.2.3.4")
	if got := plan.Changes(); got != 2 {
		t.Errorf("Changes() = %d, want 2", got)
	}
}

// The critical safety property: a family whose query failed must be left
// entirely alone, or a resolver outage looks like a mass extinction.
func TestDiffLookupFailureLeavesRecordsAlone(t *testing.T) {
	prev := []store.IPRecord{active("1.2.3.4", 4, 0), active("2001:db8::1", 6, 0)}

	for _, status := range []store.Status{store.StatusServFail, store.StatusTimeout, store.StatusError} {
		t.Run(string(status), func(t *testing.T) {
			plan := Diff(prev, store.StatusOK, Observation{A: fail(status), AAAA: fail(status)}, 1)
			if len(plan.Actions) != 0 {
				t.Errorf("failed lookup produced actions: %v", keys(actionSet(plan)))
			}
			if plan.Changes() != 1 { // only the status change
				t.Errorf("Changes() = %d, want 1 (status only)", plan.Changes())
			}
		})
	}
}

// A partial failure must not retire the healthy family's addresses either.
func TestDiffPerFamilyIsolation(t *testing.T) {
	prev := []store.IPRecord{active("1.2.3.4", 4, 0), active("2001:db8::1", 6, 0)}
	plan := Diff(prev, store.StatusOK, Observation{
		A:    ok("1.2.3.4"),
		AAAA: fail(store.StatusServFail),
	}, 1)

	// v4 refreshes; the v6 record is untouched despite being absent.
	wantActions(t, plan, "refresh 1.2.3.4")
}

// NXDOMAIN is authoritative, so addresses really are gone.
func TestDiffNXDomainRetiresAddresses(t *testing.T) {
	prev := []store.IPRecord{active("1.2.3.4", 4, 0), active("2001:db8::1", 6, 0)}
	plan := Diff(prev, store.StatusOK, Observation{A: nxdomain(), AAAA: nxdomain()}, 1)

	wantActions(t, plan, "remove 1.2.3.4", "remove 2001:db8::1")
	if plan.Status != store.StatusNXDomain {
		t.Errorf("status = %q, want nxdomain", plan.Status)
	}
}

// NODATA is also authoritative: the name exists but has no addresses.
func TestDiffNoDataRetiresAddresses(t *testing.T) {
	prev := []store.IPRecord{active("1.2.3.4", 4, 0)}
	plan := Diff(prev, store.StatusOK, Observation{A: nodata(), AAAA: nodata()}, 1)
	wantActions(t, plan, "remove 1.2.3.4")
}

func TestDiffMissThreshold(t *testing.T) {
	// With a threshold of 3, the first two absences only tick the counter.
	for _, tc := range []struct {
		missSoFar int
		want      string
	}{
		{0, "miss 1.2.3.4"},
		{1, "miss 1.2.3.4"},
		{2, "remove 1.2.3.4"}, // third consecutive absence retires it
		{5, "remove 1.2.3.4"},
	} {
		prev := []store.IPRecord{active("1.2.3.4", 4, tc.missSoFar)}
		plan := Diff(prev, store.StatusOK, Observation{A: nodata(), AAAA: nodata()}, 3)
		wantActions(t, plan, tc.want)
	}
}

func TestDiffMissThresholdFloor(t *testing.T) {
	// A threshold below 1 is nonsense; treat it as remove-immediately.
	prev := []store.IPRecord{active("1.2.3.4", 4, 0)}
	plan := Diff(prev, store.StatusOK, Observation{A: nodata(), AAAA: nodata()}, 0)
	wantActions(t, plan, "remove 1.2.3.4")
}

// A returning address should be added afresh, not silently refreshed.
func TestDiffAddressReturns(t *testing.T) {
	prev := []store.IPRecord{
		{IP: "1.2.3.4", Family: 4, Active: false}, // closed interval
	}
	plan := Diff(prev, store.StatusOK, Observation{A: ok("1.2.3.4"), AAAA: nodata()}, 1)
	wantActions(t, plan, "add 1.2.3.4")
}

func TestDiffRoundRobinChurnSuppressed(t *testing.T) {
	// A rotating tracker shows a different subset each poll. With a high
	// threshold the absent members only accrue misses rather than churning.
	prev := []store.IPRecord{
		active("1.1.1.1", 4, 0), active("2.2.2.2", 4, 0), active("3.3.3.3", 4, 0),
	}
	plan := Diff(prev, store.StatusOK, Observation{A: ok("1.1.1.1"), AAAA: nodata()}, 4)

	wantActions(t, plan, "refresh 1.1.1.1", "miss 2.2.2.2", "miss 3.3.3.3")
	if plan.Changes() != 0 {
		t.Errorf("Changes() = %d, want 0: misses must not reach the change feed", plan.Changes())
	}
}

func TestDiffIsDeterministic(t *testing.T) {
	prev := []store.IPRecord{active("1.1.1.1", 4, 0), active("2.2.2.2", 4, 0)}
	obs := Observation{A: ok("3.3.3.3", "1.1.1.1"), AAAA: nodata()}

	first := Diff(prev, store.StatusOK, obs, 1)
	for i := 0; i < 20; i++ {
		got := Diff(prev, store.StatusOK, obs, 1)
		if len(got.Actions) != len(first.Actions) {
			t.Fatalf("action count varies between runs")
		}
		for j := range got.Actions {
			if got.Actions[j] != first.Actions[j] {
				t.Fatalf("action order varies: %v vs %v", got.Actions, first.Actions)
			}
		}
	}
}

func TestObservationErrAndDuration(t *testing.T) {
	obs := Observation{A: fail(store.StatusServFail), AAAA: ok("2001:db8::1")}
	if obs.Err() != "servfail" {
		t.Errorf("Err() = %q, want the A-side error", obs.Err())
	}
	clean := Observation{A: ok("1.2.3.4"), AAAA: ok("2001:db8::1")}
	if got := clean.Err(); got != "" {
		t.Errorf("Err() = %q, want empty when both succeed", got)
	}
}
