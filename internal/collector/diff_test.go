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

// diffAddrs is the per-address diff: rolling detection needs RollAfter set, so
// leaving it at zero keeps these tests on the address-by-address behaviour they
// were written for.
func diffAddrs(prev []store.IPRecord, prevStatus store.Status, obs Observation, missThreshold int) store.Plan {
	return Diff(prev, nil, prevStatus, obs, Options{MissThreshold: missThreshold})
}

func TestDiffFirstObservation(t *testing.T) {
	plan := diffAddrs(nil, "", Observation{A: ok("1.2.3.4", "5.6.7.8"), AAAA: ok("2001:db8::1")}, 1)

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
	plan := diffAddrs(prev, store.StatusOK, Observation{A: ok("1.2.3.4"), AAAA: ok("2001:db8::1")}, 1)

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
	plan := diffAddrs(prev, store.StatusOK, Observation{A: ok("9.9.9.9"), AAAA: nodata()}, 1)

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
			plan := diffAddrs(prev, store.StatusOK, Observation{A: fail(status), AAAA: fail(status)}, 1)
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
	plan := diffAddrs(prev, store.StatusOK, Observation{
		A:    ok("1.2.3.4"),
		AAAA: fail(store.StatusServFail),
	}, 1)

	// v4 refreshes; the v6 record is untouched despite being absent.
	wantActions(t, plan, "refresh 1.2.3.4")
}

// NXDOMAIN is authoritative, so addresses really are gone.
func TestDiffNXDomainRetiresAddresses(t *testing.T) {
	prev := []store.IPRecord{active("1.2.3.4", 4, 0), active("2001:db8::1", 6, 0)}
	plan := diffAddrs(prev, store.StatusOK, Observation{A: nxdomain(), AAAA: nxdomain()}, 1)

	wantActions(t, plan, "remove 1.2.3.4", "remove 2001:db8::1")
	if plan.Status != store.StatusNXDomain {
		t.Errorf("status = %q, want nxdomain", plan.Status)
	}
}

// NODATA is also authoritative: the name exists but has no addresses.
func TestDiffNoDataRetiresAddresses(t *testing.T) {
	prev := []store.IPRecord{active("1.2.3.4", 4, 0)}
	plan := diffAddrs(prev, store.StatusOK, Observation{A: nodata(), AAAA: nodata()}, 1)
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
		plan := diffAddrs(prev, store.StatusOK, Observation{A: nodata(), AAAA: nodata()}, 3)
		wantActions(t, plan, tc.want)
	}
}

func TestDiffMissThresholdFloor(t *testing.T) {
	// A threshold below 1 is nonsense; treat it as remove-immediately.
	prev := []store.IPRecord{active("1.2.3.4", 4, 0)}
	plan := diffAddrs(prev, store.StatusOK, Observation{A: nodata(), AAAA: nodata()}, 0)
	wantActions(t, plan, "remove 1.2.3.4")
}

// A returning address should be added afresh, not silently refreshed.
func TestDiffAddressReturns(t *testing.T) {
	prev := []store.IPRecord{
		{IP: "1.2.3.4", Family: 4, Active: false}, // closed interval
	}
	plan := diffAddrs(prev, store.StatusOK, Observation{A: ok("1.2.3.4"), AAAA: nodata()}, 1)
	wantActions(t, plan, "add 1.2.3.4")
}

func TestDiffRoundRobinChurnSuppressed(t *testing.T) {
	// A rotating tracker shows a different subset each poll. With a high
	// threshold the absent members only accrue misses rather than churning.
	prev := []store.IPRecord{
		active("1.1.1.1", 4, 0), active("2.2.2.2", 4, 0), active("3.3.3.3", 4, 0),
	}
	plan := diffAddrs(prev, store.StatusOK, Observation{A: ok("1.1.1.1"), AAAA: nodata()}, 4)

	wantActions(t, plan, "refresh 1.1.1.1", "miss 2.2.2.2", "miss 3.3.3.3")
	if plan.Changes() != 0 {
		t.Errorf("Changes() = %d, want 0: misses must not reach the change feed", plan.Changes())
	}
}

func TestDiffIsDeterministic(t *testing.T) {
	prev := []store.IPRecord{active("1.1.1.1", 4, 0), active("2.2.2.2", 4, 0)}
	obs := Observation{A: ok("3.3.3.3", "1.1.1.1"), AAAA: nodata()}

	first := diffAddrs(prev, store.StatusOK, obs, 1)
	for i := 0; i < 20; i++ {
		got := diffAddrs(prev, store.StatusOK, obs, 1)
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

// --- rolling families --------------------------------------------------
//
// A host behind a CDN answers with a different set of edge addresses every
// time its TTL expires. Recorded address by address that is thousands of rows
// a year saying the same thing, so after RollAfter consecutive runs with a
// changed set the family is stored as the prefix the addresses sit in, and the
// individual addresses stop being written down.

// cdn is one CloudFront-shaped answer: every address in one /48.
func cdn(suffixes ...string) resolver.Result {
	addrs := make([]string, len(suffixes))
	for i, s := range suffixes {
		addrs[i] = "2600:9000:2094:" + s + "::1"
	}
	return resolver.Result{Status: store.StatusOK, Addrs: addrs}
}

// runs threads a sequence of answers through Diff, carrying the family
// bookkeeping from one run to the next the way the collector does, and returns
// the last plan. check, if set, sees every plan as it is made.
func runs(states map[int]store.FamilyState, opts Options, answers []resolver.Result,
	check func(i int, p store.Plan),
) store.Plan {
	var plan store.Plan
	for i, answer := range answers {
		plan = Diff(nil, states, store.StatusOK, Observation{A: nodata(), AAAA: answer}, opts)
		for _, st := range plan.States {
			states[st.Family] = st
		}
		if check != nil {
			check(i, plan)
		}
	}
	return plan
}

// stateFor picks one family's bookkeeping out of a plan. Both families get a
// state on every run, including the one answering NODATA, so index order is
// not something to rely on.
func stateFor(t *testing.T, p store.Plan, family int) store.FamilyState {
	t.Helper()
	for _, st := range p.States {
		if st.Family == family {
			return st
		}
	}
	t.Fatalf("no state for family %d in %+v", family, p.States)
	return store.FamilyState{}
}

// rollOpts collapses onto the /48 every cdn() address belongs to.
func rollOpts(rollAfter int) Options {
	return Options{
		MissThreshold: 1,
		RollAfter:     rollAfter,
		PrefixFor: func(ip string) string {
			if len(ip) > 15 && ip[:15] == "2600:9000:2094:" {
				return "2600:9000:2094::/48"
			}
			return ""
		},
	}
}

func TestRollingStartsAfterConsecutiveChanges(t *testing.T) {
	opts := rollOpts(3)
	states := map[int]store.FamilyState{}

	// Run 1 establishes the baseline; runs 2 and 3 each change the set. The
	// third change is what tips the family over.
	answers := []resolver.Result{cdn("1400"), cdn("3c00"), cdn("5c00"), cdn("7600")}
	plan := runs(states, opts, answers, func(i int, _ store.Plan) {
		if i < len(answers)-1 && states[6].Rolling {
			t.Fatalf("rolling after %d runs, want it to wait for %d changed runs", i+1, opts.RollAfter)
		}
	})

	if !states[6].Rolling {
		t.Fatalf("family 6 not rolling after %d changed runs: %+v", len(answers)-1, states[6])
	}
	if !stateFor(t, plan, 6).ModeChanged {
		t.Error("the switch into rolling should be reported as a mode change")
	}
	wantActions(t, plan, "add 2600:9000:2094::/48")
	if !plan.Actions[0].Prefix {
		t.Error("the added record should be marked as a prefix")
	}
}

func TestRollingSupersedesAddressRecords(t *testing.T) {
	opts := rollOpts(1)
	states := map[int]store.FamilyState{6: {Family: 6, Fingerprint: "old", Churn: 0}}
	prev := []store.IPRecord{active("2600:9000:2094:1400::1", 6, 0), active("2600:9000:2094:3c00::1", 6, 0)}

	plan := Diff(prev, states, store.StatusOK, Observation{A: nodata(), AAAA: cdn("5c00")}, opts)

	// The addresses are not gone, they are just no longer how we record this
	// family, so they close without an ip_removed entry.
	wantActions(t, plan,
		"supersede 2600:9000:2094:1400::1",
		"supersede 2600:9000:2094:3c00::1",
		"add 2600:9000:2094::/48")
	if n := plan.Changes(); n != 2 {
		t.Errorf("changes = %d, want 2: the prefix add and the mode change", n)
	}
}

func TestRollingHoldsWhileThePrefixIsUnchanged(t *testing.T) {
	opts := rollOpts(1)
	states := map[int]store.FamilyState{6: {Family: 6, Fingerprint: "old", Rolling: true}}
	prev := []store.IPRecord{{IP: "2600:9000:2094::/48", Family: 6, Active: true, IsPrefix: true}}

	plan := Diff(prev, states, store.StatusOK, Observation{A: nodata(), AAAA: cdn("a800", "ce00")}, opts)

	// A completely different set of addresses, same prefix: nothing to report.
	wantActions(t, plan, "refresh 2600:9000:2094::/48")
	if n := plan.Changes(); n != 0 {
		t.Errorf("changes = %d, want 0 while the prefix holds", n)
	}
}

func TestRollingEndsWhenAddressesSettle(t *testing.T) {
	opts := rollOpts(1)
	opts.SteadyAfter = 2
	answer := cdn("1400")
	states := map[int]store.FamilyState{
		6: {Family: 6, Fingerprint: fingerprint(answer.Addrs), Rolling: true, Steady: 1},
	}
	prev := []store.IPRecord{{IP: "2600:9000:2094::/48", Family: 6, Active: true, IsPrefix: true}}

	plan := Diff(prev, states, store.StatusOK, Observation{A: nodata(), AAAA: answer}, opts)

	if stateFor(t, plan, 6).Rolling {
		t.Error("still rolling after the address set held still")
	}
	wantActions(t, plan, "supersede 2600:9000:2094::/48", "add 2600:9000:2094:1400::1")
}

func TestRollingWaitsForEnrichment(t *testing.T) {
	opts := rollOpts(1)
	opts.PrefixFor = func(string) string { return "" } // nothing enriched yet
	states := map[int]store.FamilyState{6: {Family: 6, Fingerprint: "old"}}
	prev := []store.IPRecord{active("2600:9000:2094:1400::1", 6, 0)}

	plan := Diff(prev, states, store.StatusOK, Observation{A: nodata(), AAAA: cdn("5c00")}, opts)

	// Without a prefix to collapse onto there is nothing to do but wait: the
	// stored records are left exactly as they were.
	wantActions(t, plan)
	if st := stateFor(t, plan, 6); st.Rolling {
		t.Error("rolling with no prefix data to roll onto")
	} else if st.Churn == 0 {
		t.Error("churn should still accumulate while waiting for enrichment")
	}
}

func TestRollingOffByDefault(t *testing.T) {
	states := map[int]store.FamilyState{6: {Family: 6, Fingerprint: "old", Churn: 99}}
	plan := Diff(nil, states, store.StatusOK,
		Observation{A: nodata(), AAAA: cdn("1400")}, Options{MissThreshold: 1})

	if stateFor(t, plan, 6).Rolling {
		t.Error("rolling without RollAfter set")
	}
	wantActions(t, plan, "add 2600:9000:2094:1400::1")
}

// The case that made the change feed useless: a CDN that serves one pool for a
// couple of hours, swaps to another, and swaps back. Every swap is four
// ip_removed and four ip_added entries, forever, because the family never
// manages RollAfter changes in a row — one unchanged run used to wipe the churn
// count and send it back to the start. p4p.arenabg.com did exactly this for
// days while its IPv6 family, which churns every single run, rolled at once.
func TestRollingCatchesAlternatingPools(t *testing.T) {
	opts := rollOpts(3)
	opts.SteadyAfter = 3
	states := map[int]store.FamilyState{}

	// Two pools, each held for two runs before the swap: change, hold, change,
	// hold, ... so no two changes are ever adjacent.
	a, b := cdn("1400"), cdn("3c00")
	answers := []resolver.Result{a, a, b, b, a, a, b}
	plan := runs(states, opts, answers, nil)

	if !states[6].Rolling {
		t.Fatalf("family 6 never rolled over %d alternating runs: %+v", len(answers), states[6])
	}
	if !stateFor(t, plan, 6).Rolling {
		t.Error("the plan should report the family as rolling")
	}
}

// The other half of the same rule: a family that renumbers once and then holds
// still is not rolling, it moved. Churn has to clear on its own or every
// migration would be mistaken for a CDN.
func TestRollingIgnoresAOneOffRenumbering(t *testing.T) {
	opts := rollOpts(3)
	opts.SteadyAfter = 3
	states := map[int]store.FamilyState{}

	// One change, then long enough at the new address set to count as settled,
	// then another single change much later.
	a, b, c := cdn("1400"), cdn("3c00"), cdn("5c00")
	runs(states, opts, []resolver.Result{a, b, b, b, b, b, c, c, c, c},
		func(int, store.Plan) {
			if states[6].Rolling {
				t.Fatalf("rolling after a single renumbering: %+v", states[6])
			}
		})
	if states[6].Churn != 0 {
		t.Errorf("churn = %d after settling, want it cleared", states[6].Churn)
	}
}
