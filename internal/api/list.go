package api

import (
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/pawal/torrent-tracker/internal/store"
)

const (
	// stableUptime is what "stable" means, and the bar the per-scheme lists
	// use. Ten days of history stops a week-old name being recommended.
	stableUptime     = 0.95
	stableMinAgeDays = 10
	listWindowDays   = 30
	maxWindowDays    = 365
)

// listSpec is what a named list selects.
type listSpec struct {
	minUptime float64
	schemes   []string
	liveOnly  bool
	minAge    int
}

// listSpecFor maps a list name, or a bare uptime percentage, onto its filter.
func listSpecFor(name string) (listSpec, bool) {
	switch name {
	case "", "stable":
		return listSpec{minUptime: stableUptime, minAge: stableMinAgeDays}, true
	case "live":
		return listSpec{liveOnly: true}, true
	case "all":
		return listSpec{}, true
	case "udp":
		return listSpec{minUptime: stableUptime, schemes: []string{"udp"}}, true
	case "http":
		return listSpec{minUptime: stableUptime, schemes: []string{"http", "https"}}, true
	}
	pct, err := strconv.Atoi(name)
	if err != nil || pct < 0 || pct > 100 {
		return listSpec{}, false
	}
	return listSpec{minUptime: float64(pct) / 100}, true
}

// wants reports whether an entry belongs on this list. An uptime bar needs a
// measurement to clear it: never probed is not the same as never down.
func (sp listSpec) wants(e store.ListEntry) bool {
	if sp.liveOnly && !e.Answers {
		return false
	}
	if sp.minUptime > 0 && (!e.Uptime.Known() || e.Uptime.Share() < sp.minUptime) {
		return false
	}
	return len(sp.schemes) == 0 || slices.Contains(sp.schemes, e.Scheme)
}

// terms spells out what a list asked for, for the comment that explains an
// empty body.
func (sp listSpec) terms(days int) string {
	var want []string
	if sp.liveOnly {
		want = append(want, "answers now")
	}
	if sp.minUptime > 0 {
		want = append(want, fmt.Sprintf("%.0f%% uptime over %d days", sp.minUptime*100, days))
	}
	if len(sp.schemes) > 0 {
		want = append(want, strings.Join(sp.schemes, " or "))
	}
	if sp.minAge > 0 {
		want = append(want, fmt.Sprintf("%d days of history", sp.minAge))
	}
	if len(want) == 0 {
		return "no endpoint on record"
	}
	return "no endpoint has " + strings.Join(want, ", ")
}

// historyDays is how much history the database actually holds, taken from the
// oldest name in it. An age floor cannot ask for more than that.
func historyDays(entries []store.ListEntry, until time.Time) int {
	oldest := until
	for _, e := range entries {
		if e.Added.Before(oldest) {
			oldest = e.Added
		}
	}
	return int(until.Sub(oldest) / (24 * time.Hour))
}

// handleList serves a client-facing list: plain text, one announce URL per
// entry and blank line delimited, so the body pastes straight into a client.
func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	spec, ok := listSpecFor(r.PathValue("filter"))
	if !ok {
		s.fail(w, http.StatusBadRequest, "unknown list: use live, stable, udp, http, all or a percentage")
		return
	}
	days, err := countParam(r, "days", listWindowDays)
	if err == nil {
		spec.minAge, err = countParam(r, "min_age_days", spec.minAge)
	}
	var perAS int
	if err == nil {
		perAS, err = countParam(r, "per_as", 0)
	}
	if err != nil {
		s.fail(w, http.StatusBadRequest, err.Error())
		return
	}

	window := min(max(days, 1), maxWindowDays)
	until := time.Now().UTC()
	from := until.AddDate(0, 0, -window)
	entries, err := s.Store.ListEndpoints(r.Context(), from, until)
	if err != nil {
		s.serverError(w, err)
		return
	}

	// A floor longer than the database has existed drops every name, which is
	// how a fresh database serves an empty flagship list. Clamp it to the
	// history held so the list is the best one available, and say so.
	var notes []string
	if held := historyDays(entries, until); spec.minAge > held {
		notes = append(notes, fmt.Sprintf(
			"age floor relaxed from %d to %d days: that is all the history held", spec.minAge, held))
		spec.minAge = held
	}

	cutoff := until.AddDate(0, 0, -spec.minAge)
	withV4Only := boolParam(r, "include_ipv4_only_trackers", true)
	withV6Only := boolParam(r, "include_ipv6_only_trackers", true)
	keep := make([]store.ListEntry, 0, len(entries))
	for _, e := range entries {
		if !spec.wants(e) || (spec.minAge > 0 && e.Added.After(cutoff)) {
			continue
		}
		// Dropping the IPv4-only trackers means every entry must have an IPv6
		// address, and the other way round.
		if (!withV4Only && !e.V6) || (!withV6Only && !e.V4) {
			continue
		}
		keep = append(keep, e)
	}

	if perAS > 0 {
		nets, err := s.Store.TrackerNetworks(r.Context())
		if err != nil {
			s.serverError(w, err)
			return
		}
		keep = capPerAS(keep, nets, perAS)
	}

	if len(keep) == 0 {
		notes = append(notes, spec.terms(window))
	}

	// Comments lead the body. It is plain text meant for pasting, and a client
	// reading a tracker list ignores a # line.
	var b strings.Builder
	for _, note := range notes {
		fmt.Fprintf(&b, "# %s\n", note)
	}
	if len(notes) > 0 && len(keep) > 0 {
		b.WriteString("\n")
	}
	for _, e := range keep {
		b.WriteString(e.URL)
		b.WriteString("\n\n")
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write([]byte(b.String()))
}

// capPerAS keeps at most quota trackers per origin AS, best uptime first, so a
// list is not forty names behind one CDN. Endpoints of one tracker count once,
// and a tracker with no known network is never dropped: nothing says it adds
// concentration.
func capPerAS(entries []store.ListEntry, nets map[int64][]store.NetworkRef, quota int) []store.ListEntry {
	count := map[int]int{}
	verdict := map[int64]bool{}
	out := make([]store.ListEntry, 0, len(entries))
	for _, e := range entries {
		keep, decided := verdict[e.TrackerID]
		if !decided {
			keep = admit(nets[e.TrackerID], count, quota)
			verdict[e.TrackerID] = keep
		}
		if keep {
			out = append(out, e)
		}
	}
	return out
}

// admit takes a tracker only when every AS it sits in is still under quota,
// charging all of them once it does.
func admit(refs []store.NetworkRef, count map[int]int, quota int) bool {
	asns := make([]int, 0, len(refs))
	for _, ref := range refs {
		if ref.ASN != 0 && !slices.Contains(asns, ref.ASN) {
			asns = append(asns, ref.ASN)
		}
	}
	for _, asn := range asns {
		if count[asn] >= quota {
			return false
		}
	}
	for _, asn := range asns {
		count[asn]++
	}
	return true
}

// countParam reads a non-negative integer, absent meaning def. Unlike intParam
// it accepts 0, since min_age_days=0 must turn the age filter off rather than
// restore its default, and it rejects garbage instead of ignoring it.
func countParam(r *http.Request, name string, def int) (int, error) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%s must be a non-negative integer", name)
	}
	return n, nil
}

// boolParam follows newTrackon, whose flags are on unless spelled off.
func boolParam(r *http.Request, name string, def bool) bool {
	v := strings.ToLower(r.URL.Query().Get(name))
	if v == "" {
		return def
	}
	return v != "false" && v != "0"
}
