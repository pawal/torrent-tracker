package collector

import (
	"fmt"
	"hash/fnv"
	"sort"
	"strings"

	"github.com/pawal/torrent-tracker/internal/resolver"
	"github.com/pawal/torrent-tracker/internal/store"
)

// Observation is one tracker's pair of lookups.
type Observation struct {
	A    resolver.Result
	AAAA resolver.Result
}

// Duration is the total time both lookups took.
func (o Observation) Duration() (d int64) {
	return o.A.RTT.Milliseconds() + o.AAAA.RTT.Milliseconds()
}

// Err returns the first lookup error, if any.
func (o Observation) Err() string {
	if o.A.Err != "" {
		return o.A.Err
	}
	return o.AAAA.Err
}

// Status collapses both families into one tracker status. An answer beats a
// non-answer; NXDOMAIN is name-level so it outranks a transport failure; a
// failure outranks NODATA because a failed query tells us nothing.
func (o Observation) Status() store.Status {
	results := [2]resolver.Result{o.A, o.AAAA}
	for _, r := range results {
		if r.Status == store.StatusOK {
			return store.StatusOK
		}
	}
	for _, r := range results {
		if r.Status == store.StatusNXDomain {
			return store.StatusNXDomain
		}
	}
	// Report the most serious failure, if either query failed.
	for _, want := range []store.Status{store.StatusServFail, store.StatusTimeout, store.StatusError} {
		for _, r := range results {
			if r.Status == want {
				return want
			}
		}
	}
	return store.StatusNoData
}

// Options tune a diff.
type Options struct {
	// MissThreshold is how many consecutive absences retire an address.
	MissThreshold int
	// RollAfter is how many changed runs switch a family to prefixes; 0 is off.
	RollAfter int
	// SteadyAfter is how many unchanged runs switch it back.
	SteadyAfter int
	// PrefixFor returns the prefix an address sits in, or "" if unknown.
	PrefixFor func(ip string) string
}

func (o Options) missThreshold() int {
	if o.MissThreshold < 1 {
		return 1
	}
	return o.MissThreshold
}

func (o Options) steadyAfter() int {
	if o.SteadyAfter < 1 {
		return 3
	}
	return o.SteadyAfter
}

// Diff decides what to do with a tracker's stored addresses. It is pure;
// timestamping and persistence happen in the store.
//
// A family whose query failed is left alone, or a resolver hiccup looks like
// every tracker losing its addresses at once. A family that keeps changing is
// switched to one record per prefix, which is all a CDN edge rotation says.
func Diff(prev []store.IPRecord, states map[int]store.FamilyState, prevStatus store.Status,
	obs Observation, opts Options,
) store.Plan {
	status := obs.Status()
	plan := store.Plan{
		Status:        status,
		PrevStatus:    prevStatus,
		StatusChanged: status != prevStatus,
		LookupErr:     obs.Err(),
	}

	for _, fam := range []struct {
		rr     resolver.RRType
		result resolver.Result
	}{
		{resolver.TypeA, obs.A},
		{resolver.TypeAAAA, obs.AAAA},
	} {
		if !fam.result.Status.Resolved() {
			continue // no trustworthy answer for this family; leave it untouched
		}
		family := fam.rr.Family()

		state := states[family]
		state.Family = family
		rolling := rollingDecision(&state, fam.result.Addrs, opts)

		target := fam.result.Addrs
		if rolling {
			target = prefixesOf(fam.result.Addrs, opts.PrefixFor)
			if len(target) == 0 {
				// Not enriched yet, so there is no prefix to collapse onto.
				state.Rolling = states[family].Rolling
				state.ModeChanged = false
				plan.States = append(plan.States, state)
				continue
			}
			state.Detail = fmt.Sprintf("%s, %d addresses per run",
				strings.Join(target, " "), len(fam.result.Addrs))
		}

		plan.Actions = append(plan.Actions,
			supersede(prev, family, rolling, state.ModeChanged)...)
		plan.Actions = append(plan.Actions,
			reconcile(prev, family, target, rolling, state.ModeChanged, opts.missThreshold())...)
		plan.States = append(plan.States, state)
	}

	return plan
}

// rollingDecision updates one family's churn counters and reports whether it
// should now be tracked by prefix.
func rollingDecision(state *store.FamilyState, addrs []string, opts Options) bool {
	fp := fingerprint(addrs)
	first := state.Fingerprint == ""
	switch {
	case first || fp == state.Fingerprint:
		state.Steady++
		state.Churn = 0
	default:
		state.Churn++
		state.Steady = 0
	}
	state.Fingerprint = fp

	was := state.Rolling
	switch {
	case opts.RollAfter <= 0:
		state.Rolling = false
	case !state.Rolling && state.Churn >= opts.RollAfter:
		state.Rolling = true
	case state.Rolling && state.Steady >= opts.steadyAfter():
		state.Rolling = false
	}
	state.ModeChanged = state.Rolling != was
	if state.ModeChanged && !state.Rolling {
		state.Detail = "addresses settled"
	}
	return state.Rolling
}

// supersede closes the records left behind by a switch of tracking mode.
func supersede(prev []store.IPRecord, family int, rolling, modeChanged bool) []store.Action {
	if !modeChanged {
		return nil
	}
	var out []store.Action
	for _, r := range prev {
		// Replace whatever was written in the mode we just left.
		if !r.Active || r.Family != family || r.IsPrefix == rolling {
			continue
		}
		out = append(out, store.Action{IP: r.IP, Family: family, Kind: store.ActionSupersede, Prefix: r.IsPrefix})
	}
	return out
}

// reconcile diffs the target set against what is already active in that mode.
func reconcile(prev []store.IPRecord, family int, target []string, rolling, modeChanged bool, missThreshold int) []store.Action {
	observed := make(map[string]bool, len(target))
	for _, v := range target {
		observed[v] = true
	}

	active := make(map[string]store.IPRecord, len(prev))
	for _, r := range prev {
		if r.Active && r.Family == family && r.IsPrefix == rolling {
			active[r.IP] = r
		}
	}

	out := make([]store.Action, 0, len(target))
	for _, v := range target {
		kind := store.ActionAdd
		if _, ok := active[v]; ok {
			kind = store.ActionRefresh
		}
		out = append(out, store.Action{IP: v, Family: family, Kind: kind, Prefix: rolling})
	}

	// Iterate prev, not the map, so the plan is deterministic. A mode change
	// has already superseded the old records, so nothing is left to retire.
	if modeChanged {
		return out
	}
	for _, r := range prev {
		if !r.Active || r.Family != family || r.IsPrefix != rolling || observed[r.IP] {
			continue
		}
		kind := store.ActionMiss
		if r.MissCount+1 >= missThreshold {
			kind = store.ActionRemove
		}
		out = append(out, store.Action{IP: r.IP, Family: family, Kind: kind, Prefix: rolling})
	}
	return out
}

// prefixesOf maps addresses to their distinct known prefixes.
func prefixesOf(addrs []string, prefixFor func(string) string) []string {
	if prefixFor == nil {
		return nil
	}
	seen := map[string]bool{}
	out := []string{}
	for _, ip := range addrs {
		p := prefixFor(ip)
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// fingerprint identifies an address set independent of answer order.
func fingerprint(addrs []string) string {
	if len(addrs) == 0 {
		return "empty"
	}
	sorted := append([]string(nil), addrs...)
	sort.Strings(sorted)
	h := fnv.New64a()
	for _, a := range sorted {
		h.Write([]byte(a))
		h.Write([]byte{0})
	}
	return fmt.Sprintf("%016x", h.Sum64())
}
