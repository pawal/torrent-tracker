package api

import (
	"cmp"
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/pawal/torrent-tracker/internal/prober"
	"github.com/pawal/torrent-tracker/internal/store"
)

// How much history the no-JS page carries. The UI asks for 200 changes; a page
// meant to be read in a terminal wants less.
const (
	fallbackChanges = 50
	fallbackRecords = 40
	fallbackLimit   = 20
)

// fallbackDoc is the page for a client that runs no JS: the present state and
// the feed, never the windowed history, which costs a month-wide scan a page.
func (s *Server) fallbackDoc(ctx context.Context, status int, path, country string, t *store.Tracker) doc {
	_, intro := pageMeta(path, country, t)
	d := doc{
		Title:  pageHeading(path, country, t),
		Intro:  intro,
		Nav:    navCells(path),
		Footer: footerCells(),
	}

	var err error
	switch {
	case status != http.StatusOK:
		d.Sections = append(d.Sections, section{
			Heading: "No such page",
			Notes:   []string{"Nothing is served at this address. The pages above are all of them."},
		})
	case t != nil:
		err = s.trackerSections(ctx, &d, *t)
	case path == pathTrackers:
		err = s.trackerListSections(ctx, &d, country)
	case path == pathNetworks:
		err = s.networkSections(ctx, &d)
	default:
		err = s.dashboardSections(ctx, &d)
	}
	if err != nil {
		// The metadata is already right, so serve the page without its body
		// rather than turning a read failure into a 500.
		s.logger().Error("render fallback", "path", path, "err", err)
		d.Sections = append(d.Sections, section{
			Heading: "Unavailable",
			Notes:   []string{"This page's data could not be read. The JSON API may still answer."},
		})
	}
	return d
}

// pageHeading is the h1. Shorter than the title, which carries the site name
// for a tab and a search result.
func pageHeading(path, country string, t *store.Tracker) string {
	switch {
	case t != nil:
		return t.Name
	case path == pathTrackers && country == "unknown":
		return "Trackers with no country on record"
	case path == pathTrackers && country != "":
		return "Trackers in " + country
	case path == pathTrackers:
		return "Known trackers"
	case path == pathNetworks:
		return "Networks"
	case path == pathDashboard:
		return "torrent-tracker"
	}
	return "Not found"
}

// navCells links the other pages. The current one is named but not linked.
func navCells(path string) []cell {
	pages := []struct{ label, href string }{
		{"Changes", pathDashboard},
		{"Trackers", pathTrackers},
		{"Networks", pathNetworks},
	}
	out := make([]cell, 0, len(pages))
	for _, p := range pages {
		if p.href == path {
			out = append(out, txt(p.label))
			continue
		}
		out = append(out, link(p.label, p.href))
	}
	return out
}

// footerCells points at what a terminal reader most likely came for.
func footerCells() []cell {
	return []cell{
		txt("Plain text: add ?format=txt to any page, or send an Accept header without text/html."),
		link("Announce lists", "/api/list"),
		link("JSON API", "/api/trackers"),
		link("Source", repoURL),
	}
}

// dashboardSections mirrors Dashboard.svelte: the counters, the resolution
// rollup and the change feed.
func (s *Server) dashboardSections(ctx context.Context, d *doc) error {
	stats, err := s.Store.Stats(ctx)
	if err != nil {
		return err
	}
	changes, err := s.Store.RecentChanges(ctx, time.Time{}, fallbackChanges)
	if err != nil {
		return err
	}

	sum := section{Heading: "Summary", Defs: []def{
		{"Trackers tracked", itoa(stats.EnabledTrackers)},
		{"Addresses live now", itoa(stats.ActiveIPs)},
		{"Tracker-address pairs", itoa(stats.ActiveIPRecords)},
		{"Addresses ever seen", itoa(stats.TotalIPs)},
		{"Changes recorded", itoa(stats.Changes)},
	}}
	for _, kv := range []def{
		{"Parked, not trackers", itoa(stats.Parked)},
		{"Never answered", itoa(stats.NeverAnswered)},
		{"Answered once, now dead", itoa(stats.WentQuiet)},
	} {
		if kv.Value != "0" {
			sum.Defs = append(sum.Defs, kv)
		}
	}
	d.Sections = append(d.Sections, sum)

	if len(stats.ByStatus) > 0 {
		res := section{Heading: "Resolution status", Table: &table{Head: []string{"Status", "Names"}}}
		for _, st := range sortedStatuses(stats.ByStatus) {
			res.Table.Rows = append(res.Table.Rows, []cell{txt(st), txt(itoa(stats.ByStatus[store.Status(st)]))})
		}
		if r := stats.LastRun; r != nil {
			res.Notes = append(res.Notes, fmt.Sprintf(
				"Last collection %s: %d resolved, %d failed, %d changes.",
				stamp(r.StartedAt), r.OKCount, r.ErrorCount, r.ChangeCount))
		}
		d.Sections = append(d.Sections, res)
	}

	feed := section{Heading: "Recent changes"}
	if len(changes) == 0 {
		feed.Notes = []string{"Nothing recorded yet. Run `trackerd poll`."}
		d.Sections = append(d.Sections, feed)
		return nil
	}
	feed.Notes = []string{fmt.Sprintf("The newest %d of %d. /api/changes carries the rest.",
		len(changes), stats.Changes)}
	feed.Table = &table{Head: []string{"When", "Tracker", "Change"}}
	for _, c := range changes {
		feed.Table.Rows = append(feed.Table.Rows, []cell{
			txt(stamp(c.ObservedAt)),
			link(c.Tracker, trackerHref(c.Tracker)),
			txt(describeChange(c)),
		})
	}
	d.Sections = append(d.Sections, feed)
	return nil
}

// trackerListSections mirrors Trackers.svelte, minus the addresses and the
// uptime. Both are on the tracker's own page, and uptime costs the scan above.
func (s *Server) trackerListSections(ctx context.Context, d *doc, country string) error {
	views, err := s.Store.ListTrackerViews(ctx, false)
	if err != nil {
		return err
	}

	sec := section{Table: &table{Head: []string{"Tracker", "DNS", "Answers", "Network"}}}
	for _, v := range views {
		if country != "" && !servedFrom(v, country) {
			continue
		}
		sec.Table.Rows = append(sec.Table.Rows, []cell{
			link(v.Name, trackerHref(v.Name)),
			txt(dnsLabel(v.Tracker)),
			txt(answersLabel(v.Tracker)),
			txt(networksLabel(v.Networks)),
		})
	}

	n := len(sec.Table.Rows)
	if country != "" {
		sec.Notes = append(sec.Notes, fmt.Sprintf("%d of %d tracked names.", n, len(views)))
	} else {
		sec.Notes = append(sec.Notes, fmt.Sprintf("%d tracked names.", n))
	}
	sec.Notes = append(sec.Notes, "Addresses, endpoints and uptime are on each tracker's own page, "+
		"and the AS holders on /networks; /api/trackers carries the lot at once.")
	if n == 0 {
		sec.Table = nil
	}
	d.Sections = append(d.Sections, sec)
	return nil
}

// networkSections mirrors Networks.svelte.
func (s *Server) networkSections(ctx context.Context, d *doc) error {
	cov, err := s.Store.Coverage(ctx)
	if err != nil {
		return err
	}
	probes, err := s.Store.ProbeCoverage(ctx)
	if err != nil {
		return err
	}
	reach, err := s.Store.ReachSummary(ctx)
	if err != nil {
		return err
	}
	networks, err := s.Store.TopNetworks(ctx, fallbackLimit)
	if err != nil {
		return err
	}
	rirs, err := s.Store.ByRIR(ctx)
	if err != nil {
		return err
	}
	countries, err := s.Store.ByCountry(ctx, fallbackLimit)
	if err != nil {
		return err
	}
	software, err := s.Store.SoftwareStats(ctx, fallbackLimit)
	if err != nil {
		return err
	}
	shared, err := s.Store.SharedAddresses(ctx, time.Now().UTC().Add(-sharedWindow), fallbackLimit)
	if err != nil {
		return err
	}

	answering := reach[store.ReachLive] + reach[store.ReachPartial]
	sec := section{Heading: "Tracker reachability", Notes: []string{fmt.Sprintf(
		"%d of %d names answer the tracker protocol, across %d announce endpoints on %d names. "+
			"Resolving in DNS is a separate question, and a good deal more of them manage that.",
		answering, probes.Trackers, probes.Endpoints, probes.WithEndpoints)}}
	if len(reach) > 0 {
		sec.Table = &table{Head: []string{"Reachability", "Names"}}
		for _, r := range sortedReach(reach) {
			sec.Table.Rows = append(sec.Table.Rows, []cell{txt(r), txt(itoa(reach[store.Reach(r)]))})
		}
	}
	if probes.Probed == 0 {
		sec.Notes = append(sec.Notes, "Run `trackerd probe` to populate this.")
	} else {
		if probes.WithEndpoints < probes.Trackers {
			sec.Notes = append(sec.Notes, fmt.Sprintf(
				"%d names were added without an announce endpoint and cannot be probed; they count as unknown.",
				probes.Trackers-probes.WithEndpoints))
		}
		if probes.NeverResolved > 0 {
			sec.Notes = append(sec.Notes, fmt.Sprintf(
				"%d have never resolved to an address at all, so there has never been anything to probe. "+
					"They are retried daily and retired after a month, history kept.", probes.NeverResolved))
		}
	}
	d.Sections = append(d.Sections, sec)

	if len(software) > 0 {
		sw := section{Heading: "By tracker software", Notes: []string{fmt.Sprintf(
			"Fingerprinted for %d of %d trackers, and %d of those left a fingerprint that names the "+
				"software. A named row was matched against the string in that project's own source; "+
				"the rest are the literal a tracker answered with. UDP endpoints disclose nothing, "+
				"so this covers the HTTP ones.",
			probes.Fingerprinted, probes.Trackers, probes.Named)},
			Table: &table{Head: []string{"Software", "Evidence", "Trackers", "Endpoints"}}}
		for _, x := range software {
			sw.Table.Rows = append(sw.Table.Rows, []cell{
				txt(cmp.Or(x.Name, x.Signature)),
				txt(describeEvidence(x.Kind)),
				txt(itoa(x.Trackers)),
				txt(itoa(x.Endpoints)),
			})
		}
		d.Sections = append(d.Sections, sw)
	}

	if len(shared) > 0 {
		sh := section{Heading: "Shared addresses", Notes: []string{
			"Names answering on one address. One host means one operator and one outage however " +
				"different the names look; a CDN edge means only one front end. Counted over the last two days."},
			Table: &table{Head: []string{"Address", "Names", "Network", "Trackers"}}}
		for _, a := range shared {
			ip := a.IP
			if !a.Active {
				ip += " (was)"
			}
			sh.Table.Rows = append(sh.Table.Rows, []cell{
				txt(ip),
				txt(itoa(len(a.Trackers))),
				txt(describeNetwork(a.Network)),
				txt(strings.Join(a.Trackers, " ")),
			})
		}
		d.Sections = append(d.Sections, sh)
	}

	cvg := section{Heading: "Enrichment coverage", Notes: []string{fmt.Sprintf(
		"%d of %d live addresses looked up, %d with an origin AS.",
		cov.Enriched, cov.ActiveIPs, cov.WithASN)}}
	if cov.Enriched == 0 {
		cvg.Notes = append(cvg.Notes, "Run `trackerd enrich` to populate this.")
		d.Sections = append(d.Sections, cvg)
		return nil
	}
	d.Sections = append(d.Sections, cvg)

	top := section{Heading: "Top networks",
		Table: &table{Head: []string{"AS", "Holder", "Trackers", "Addresses"}}}
	for _, n := range networks {
		top.Table.Rows = append(top.Table.Rows, []cell{
			txt(n.Key), txt(cmp.Or(n.Label, "-")), txt(itoa(n.Trackers)), txt(itoa(n.IPs)),
		})
	}
	d.Sections = append(d.Sections, top)

	byRIR := section{Heading: "By RIR",
		Table: &table{Head: []string{"Registry", "Trackers", "Addresses"}}}
	for _, r := range rirs {
		byRIR.Table.Rows = append(byRIR.Table.Rows,
			[]cell{txt(r.Key), txt(itoa(r.Trackers)), txt(itoa(r.IPs))})
	}
	d.Sections = append(d.Sections, byRIR)

	byCC := section{Heading: "By country",
		Notes: []string{"Each country links to the trackers served from it."},
		Table: &table{Head: []string{"Country", "Trackers", "Addresses"}}}
	for _, c := range countries {
		byCC.Table.Rows = append(byCC.Table.Rows, []cell{
			link(c.Key, pathTrackers+"?country="+url.QueryEscape(c.Key)),
			txt(itoa(c.Trackers)), txt(itoa(c.IPs)),
		})
	}
	d.Sections = append(d.Sections, byCC)
	return nil
}

// trackerSections mirrors TrackerDetail.svelte, minus the timeline charts: the
// verdicts they plot are here as text, the intervals in /api/trackers/{name}.
func (s *Server) trackerSections(ctx context.Context, d *doc, t store.Tracker) error {
	records, err := s.Store.RecordsFor(ctx, t.ID)
	if err != nil {
		return err
	}
	info, err := s.Store.IPInfoForTracker(ctx, t.ID)
	if err != nil {
		return err
	}
	endpoints, err := s.Store.EndpointsFor(ctx, t.ID)
	if err != nil {
		return err
	}
	probes, err := s.Store.ProbesFor(ctx, t.ID)
	if err != nil {
		return err
	}
	changes, err := s.Store.ChangesFor(ctx, t.ID, fallbackChanges)
	if err != nil {
		return err
	}

	st := section{Heading: "Status", Defs: []def{
		{"Answers", answersLabel(t)},
		{"DNS", dnsLabel(t)},
		{"Source", cmp.Or(t.Source, "unknown")},
		{"Added", cmp.Or(isoDate(t.CreatedAt), "unknown")},
		{"Last resolved", stampOr(t.LastCheckedAt, "never")},
		{"Last probed", stampOr(t.ReachCheckedAt, "never")},
		{"Last answered", stampOr(t.LastLiveAt, "never")},
	}}
	if t.BEP34 != "" {
		st.Defs = append(st.Defs, def{"BEP 34", t.BEP34})
	}
	if !t.Enabled {
		st.Defs = append(st.Defs, def{"Collection", "stopped; the history stays"})
	}
	st.Notes = []string{"Uptime and the per-address timelines are in /api/trackers/" + t.Name + "."}
	d.Sections = append(d.Sections, st)

	d.Sections = append(d.Sections, endpointSection(t, endpoints, probes))
	d.Sections = append(d.Sections, addressSection(records, info))

	log := section{Heading: "Change log"}
	if len(changes) == 0 {
		log.Notes = []string{"Nothing recorded for this name yet."}
	} else {
		log.Table = &table{Head: []string{"When", "Change"}}
		for _, c := range changes {
			log.Table.Rows = append(log.Table.Rows,
				[]cell{txt(stamp(c.ObservedAt)), txt(describeChange(c))})
		}
	}
	d.Sections = append(d.Sections, log)
	return nil
}

// endpointSection is the per-address verdict for each announce endpoint. A name
// can resolve perfectly and answer nothing, which is why this is not the DNS one.
func endpointSection(t store.Tracker, endpoints []store.Endpoint, probes []store.Probe) section {
	sec := section{Heading: "Tracker protocol"}
	switch {
	case t.BEP34Denies:
		sec.Notes = []string{t.BEP34 + " — this host publishes a BEP 34 record naming no tracker, " +
			"which is the operator asking not to be contacted. It is no longer probed. " +
			"The name and everything measured before stay on record."}
		return sec
	case len(endpoints) == 0:
		sec.Notes = []string{"No announce endpoint on record, so there is nothing to speak to. " +
			"This name was added bare; re-import the list it came from to pick its endpoints up."}
		return sec
	}
	if t.BEP34 != "" {
		sec.Notes = append(sec.Notes, "Advertises "+t.BEP34+" in DNS.")
	}
	sec.Notes = append(sec.Notes, "Whether the tracker answers, checked per address.")

	sec.Table = &table{Head: []string{"Endpoint", "Address", "Answers", "Software", "Detail", "Since"}}
	for _, e := range endpoints {
		rows := 0
		for _, p := range probes {
			if p.EndpointID != e.ID {
				continue
			}
			rows++
			sec.Table.Rows = append(sec.Table.Rows, []cell{
				txt(e.Label()), txt(p.IP), txt(string(p.Result)),
				txt(cmp.Or(p.Software, p.Signature, "-")),
				txt(probeDetail(p)),
				txt(stamp(p.Since)),
			})
		}
		if rows > 0 {
			continue
		}
		sec.Table.Rows = append(sec.Table.Rows, []cell{
			txt(e.Label()), txt("-"), txt("-"), txt("-"), txt(endpointNote(t, e)), txt("-"),
		})
	}
	return sec
}

func endpointNote(t store.Tracker, e store.Endpoint) string {
	if e.RetiredAt != nil {
		return "retired " + stamp(*e.RetiredAt) + " — advertised in DNS and answering under neither scheme"
	}
	if t.LastStatus == store.StatusOK {
		return "not probed yet"
	}
	return "not probed yet (nothing resolved to probe)"
}

func probeDetail(p store.Probe) string {
	switch {
	case p.Reason != "":
		return p.Reason
	case p.RTTms > 0:
		return itoa(p.RTTms) + " ms"
	}
	return "-"
}

// addressSection is the address history, live intervals first. A rolling
// family shows the prefix it was collapsed to, not the addresses inside it.
func addressSection(records []store.IPRecord, info map[string]store.IPInfo) section {
	sec := section{Heading: "Address history"}
	if len(records) == 0 {
		sec.Notes = []string{"No address has ever been recorded for this name."}
		return sec
	}
	if len(records) > fallbackRecords {
		sec.Notes = append(sec.Notes, fmt.Sprintf(
			"The newest %d of %d intervals. /api/trackers/{name} carries the rest.",
			fallbackRecords, len(records)))
		records = records[:fallbackRecords]
	}

	sec.Table = &table{Head: []string{"Address", "State", "First seen", "Last seen", "Network"}}
	for _, r := range records {
		state := "gone"
		if r.Active {
			state = "active"
		}
		if r.IsPrefix {
			state += ", prefix"
		}
		sec.Table.Rows = append(sec.Table.Rows, []cell{
			txt(r.IP), txt(state), txt(stamp(r.FirstSeen)), txt(stamp(r.LastSeen)),
			txt(addressNetwork(info[r.IP])),
		})
	}
	return sec
}

func addressNetwork(i store.IPInfo) string {
	n := describeNetwork(store.NetworkRef{ASN: i.ASN, Holder: i.Holder(), RIR: i.RIR, Country: i.Country})
	if n == "" {
		return "-"
	}
	return n
}

// dnsLabel is the resolver's verdict and nothing else, plus the two facts that
// say a name is not a tracker however well it resolves.
func dnsLabel(t store.Tracker) string {
	out := cmp.Or(string(t.LastStatus), "unchecked")
	if t.Parked {
		out += ", parked"
	}
	if t.BEP34Denies {
		out += ", denies"
	}
	return out
}

// answersLabel says whether the tracker protocol replies. Never answered and
// gone quiet are different names: only the second was ever a tracker.
func answersLabel(t store.Tracker) string {
	switch {
	case t.Reach == store.ReachLive:
		return "live"
	case t.Reach == store.ReachPartial:
		return "partial"
	case t.Reach == store.ReachDead && t.LastLiveAt == nil:
		return "never answered"
	case t.Reach == store.ReachDead:
		return "silent"
	}
	return "unprobed"
}

// servedFrom mirrors inCountry in web/src/lib/api.js: one active address in the
// country is enough, as the rollup counts it.
func servedFrom(v store.TrackerView, country string) bool {
	want := strings.ToLower(country)
	for _, n := range v.Networks {
		cc := n.Country
		if cc == "" {
			cc = "unknown"
		}
		if strings.ToLower(cc) == want {
			return true
		}
	}
	return false
}

// networksLabel is the AS and country of every network a name sits in. The
// holder is left out: it runs to fifty columns and /networks names it.
func networksLabel(refs []store.NetworkRef) string {
	out := make([]string, 0, len(refs))
	for _, n := range refs {
		var parts []string
		if n.ASN != 0 {
			parts = append(parts, "AS"+itoa(n.ASN))
		}
		if n.Country != "" {
			parts = append(parts, n.Country)
		}
		if len(parts) > 0 {
			out = append(out, strings.Join(parts, " "))
		}
	}
	if len(out) == 0 {
		return "-"
	}
	return strings.Join(out, "; ")
}

// Cymru AS names read "CLOUDFLARENET - Cloudflare, Inc., US". Drop the handle
// prefix and the trailing country; both are shown elsewhere.
var (
	asHandle  = regexp.MustCompile(`^[A-Z0-9_-]+\s+-\s+`)
	asCountry = regexp.MustCompile(`,\s*[A-Z]{2}$`)
)

// describeNetwork mirrors describeNetwork in web/src/lib/api.js.
func describeNetwork(n store.NetworkRef) string {
	var parts []string
	if n.ASN != 0 {
		parts = append(parts, "AS"+itoa(n.ASN))
	}
	if h := asCountry.ReplaceAllString(asHandle.ReplaceAllString(n.Holder, ""), ""); h != "" {
		parts = append(parts, h)
	}
	return strings.Join(parts, " ")
}

// describeEvidence mirrors describeEvidence in web/src/lib/api.js.
func describeEvidence(k prober.Kind) string {
	switch k {
	case prober.KindFailure:
		return "failure text"
	case prober.KindShape:
		return "reply shape"
	}
	return "-"
}

// describeChange renders a feed entry the way the original Perl report did:
// a sign and a reason. Mirrors describe in web/src/lib/api.js.
func describeChange(c store.Change) string {
	switch c.Type {
	case store.ChangeIPAdded:
		return "+ " + c.IP
	case store.ChangeIPRemoved:
		return "- " + c.IP
	case store.ChangePrefixAdded:
		return "+ " + c.IP + " (prefix)"
	case store.ChangePrefixRemoved:
		return "- " + c.IP + " (prefix)"
	case store.ChangeStatusChanged:
		return "! " + c.Detail
	case store.ChangeASNChanged:
		return "~ " + c.Detail
	case store.ChangeTrackerAdded:
		if c.Detail == "" {
			return "* added"
		}
		return "* added (" + c.Detail + ")"
	case store.ChangeTrackerRetired:
		return "* retired — " + c.Detail
	case store.ChangeIPsRolling:
		return fmt.Sprintf("~ IPv%d rolls: %s", c.Family, c.Detail)
	case store.ChangeIPsStable:
		return fmt.Sprintf("~ IPv%d %s", c.Family, c.Detail)
	case store.ChangeParked:
		return "! " + cmp.Or(c.Detail, "parked")
	case store.ChangeBEP34Added:
		return "~ publishes " + c.Detail
	case store.ChangeBEP34Removed:
		return "~ withdrew " + c.Detail
	case store.ChangeBEP34Changed:
		return "~ preferences " + c.Detail
	case store.ChangeTrackerUp:
		return "^ answering again — " + c.Detail
	case store.ChangeTrackerDown:
		return "v stopped answering — " + c.Detail
	case store.ChangeTrackerPartial:
		return "~ partly answering — " + c.Detail
	}
	return strings.TrimSpace("? " + c.Type + " " + c.Detail)
}

// Worst first, so a wall of "ok" never buries the interesting statuses.
var statusOrder = []string{"nxdomain", "servfail", "timeout", "error", "nodata", "unchecked", "ok"}

func sortedStatuses(byStatus map[store.Status]int) []string {
	out := make([]string, 0, len(byStatus))
	for st := range byStatus {
		out = append(out, string(st))
	}
	return sortByOrder(out, statusOrder)
}

var reachOrder = []string{"live", "partial", "dead", "unknown"}

func sortedReach(byReach map[store.Reach]int) []string {
	out := make([]string, 0, len(byReach))
	for r := range byReach {
		out = append(out, string(r))
	}
	return sortByOrder(out, reachOrder)
}

// sortByOrder ranks by want, keys it does not name sorting after the rest.
func sortByOrder(keys, want []string) []string {
	rank := func(k string) int {
		if i := slices.Index(want, k); i >= 0 {
			return i
		}
		return len(want)
	}
	slices.SortFunc(keys, func(a, b string) int {
		if d := rank(a) - rank(b); d != 0 {
			return d
		}
		return strings.Compare(a, b)
	})
	return keys
}

func trackerHref(name string) string { return trackerPrefix + url.PathEscape(name) }

func itoa(n int) string { return strconv.Itoa(n) }

// stamp mirrors fmtTime in web/src/lib/api.js.
func stamp(t time.Time) string { return t.UTC().Format("2006-01-02 15:04:05Z") }

func stampOr(t *time.Time, alt string) string {
	if t == nil {
		return alt
	}
	return stamp(*t)
}
