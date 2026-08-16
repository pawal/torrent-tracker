package collector

import (
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

// Diff decides what to do with a tracker's stored addresses. It is pure;
// timestamping and persistence happen in the store.
//
// A family whose query failed is left alone, or a resolver hiccup looks like
// every tracker losing its addresses at once. missThreshold is how many
// consecutive absences retire an address, which damps round-robin churn.
func Diff(prev []store.IPRecord, prevStatus store.Status, obs Observation, missThreshold int) store.Plan {
	if missThreshold < 1 {
		missThreshold = 1
	}
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

		observed := make(map[string]bool, len(fam.result.Addrs))
		for _, ip := range fam.result.Addrs {
			observed[ip] = true
		}

		active := make(map[string]store.IPRecord, len(prev))
		for _, r := range prev {
			if r.Active && r.Family == family {
				active[r.IP] = r
			}
		}

		// Addresses we can see now.
		for _, ip := range fam.result.Addrs {
			if _, ok := active[ip]; ok {
				plan.Actions = append(plan.Actions, store.Action{IP: ip, Family: family, Kind: store.ActionRefresh})
			} else {
				plan.Actions = append(plan.Actions, store.Action{IP: ip, Family: family, Kind: store.ActionAdd})
			}
		}

		// Addresses we expected but did not see. Iterate prev (ordered) rather
		// than the map so the plan is deterministic.
		for _, r := range prev {
			if !r.Active || r.Family != family || observed[r.IP] {
				continue
			}
			kind := store.ActionMiss
			if r.MissCount+1 >= missThreshold {
				kind = store.ActionRemove
			}
			plan.Actions = append(plan.Actions, store.Action{IP: r.IP, Family: family, Kind: kind})
		}
	}

	return plan
}
