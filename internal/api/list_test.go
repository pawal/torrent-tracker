package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pawal/torrent-tracker/internal/store"
)

// day is how far back the list fixtures reach; the default window is 30 days,
// so everything here sits comfortably inside it.
const day = 24 * time.Hour

// listTracker seeds a name a client list could pick up: a creation date, one
// announce endpoint and the addresses it resolves to.
func listTracker(t *testing.T, st *store.Store, name string, added time.Time, scheme string, port int, ips ...string) (int64, int64) {
	t.Helper()
	ctx := t.Context()

	tr, _, err := st.AddTracker(ctx, name, "test", added)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddEndpoint(ctx, tr.ID, scheme, port, "/announce", added); err != nil {
		t.Fatal(err)
	}
	actions := make([]store.Action, 0, len(ips))
	for _, ip := range ips {
		actions = append(actions, store.Action{IP: ip, Family: store.Family(ip), Kind: store.ActionAdd})
	}
	if err := st.ApplyPlan(ctx, tr.ID, store.Plan{
		Status: store.StatusOK, StatusChanged: true, Actions: actions,
	}, added); err != nil {
		t.Fatal(err)
	}
	eps, err := st.EndpointsFor(ctx, tr.ID)
	if err != nil {
		t.Fatal(err)
	}
	return tr.ID, eps[0].ID
}

// putVerdict records one probe result standing from since onwards, archiving
// whatever it replaced.
func putVerdict(t *testing.T, st *store.Store, endpointID int64, ip string, result store.ProbeResult, since time.Time) {
	t.Helper()
	p := store.Probe{
		EndpointID: endpointID, IP: ip, Family: store.Family(ip),
		Result: result, Since: since, CheckedAt: since,
	}
	if err := st.PutProbes(t.Context(), []int64{endpointID}, []store.Probe{p}, since); err != nil {
		t.Fatal(err)
	}
}

// endpointOf finds a tracker's endpoint on one scheme, for the tests that add a
// second endpoint after the fixture built the first.
func endpointOf(t *testing.T, st *store.Store, trackerID int64, scheme string) int64 {
	t.Helper()
	eps, err := st.EndpointsFor(t.Context(), trackerID)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range eps {
		if e.Scheme == scheme {
			return e.ID
		}
	}
	t.Fatalf("tracker %d has no %s endpoint", trackerID, scheme)
	return 0
}

// urls reads a list response back into the announce URLs it carries, checking
// it was served as text along the way.
func urls(t *testing.T, rec *httptest.ResponseRecorder) []string {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d (body: %s)", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/plain; charset=utf-8" {
		t.Errorf("Content-Type = %q, want text/plain", ct)
	}
	out := []string{}
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

// seedLists puts up a tracker that answered throughout and one that went down
// for the last quarter of the window.
func seedLists(t *testing.T, st *store.Store) (now time.Time) {
	t.Helper()
	now = time.Now().UTC()

	_, good := listTracker(t, st, "good.example.com", now.Add(-20*day), "udp", 6969, "1.2.3.4")
	putVerdict(t, st, good, "1.2.3.4", store.ProbeLive, now.Add(-20*day))

	_, flaky := listTracker(t, st, "flaky.example.com", now.Add(-20*day), "udp", 6969, "1.2.3.5")
	putVerdict(t, st, flaky, "1.2.3.5", store.ProbeLive, now.Add(-20*day))
	putVerdict(t, st, flaky, "1.2.3.5", store.ProbeDead, now.Add(-5*day))
	return now
}

func TestListStable(t *testing.T) {
	h, st := testServer(t)
	seedLists(t, st)

	got := urls(t, get(t, h, "/api/list/stable"))
	if len(got) != 1 || got[0] != "udp://good.example.com:6969/announce" {
		t.Errorf("stable = %v, want only the tracker above 95%%", got)
	}
	// The bare path is the same list: it is the one worth recommending.
	if bare := urls(t, get(t, h, "/api/list")); len(bare) != len(got) {
		t.Errorf("/api/list = %v, want the stable list %v", bare, got)
	}
}

func TestListIsBlankLineDelimited(t *testing.T) {
	h, st := testServer(t)
	seedLists(t, st)

	// The format clients expect from a tracker list: one URL per entry with a
	// blank line after it, so the body pastes straight in.
	if body := get(t, h, "/api/list/stable").Body.String(); body != "udp://good.example.com:6969/announce\n\n" {
		t.Errorf("body = %q", body)
	}
}

func TestListPercentage(t *testing.T) {
	h, st := testServer(t)
	seedLists(t, st)

	if got := urls(t, get(t, h, "/api/list/50")); len(got) != 2 {
		t.Errorf("50%% list = %v, want both trackers", got)
	}
	if got := urls(t, get(t, h, "/api/list/0")); len(got) != 2 {
		t.Errorf("0%% list = %v, want both trackers", got)
	}
	for _, path := range []string{"/api/list/101", "/api/list/-1", "/api/list/best"} {
		if rec := get(t, h, path); rec.Code != http.StatusBadRequest {
			t.Errorf("GET %s = %d, want 400", path, rec.Code)
		}
	}
}

func TestListNeedsAMeasurementToClearTheBar(t *testing.T) {
	h, st := testServer(t)
	now := time.Now().UTC()
	listTracker(t, st, "unprobed.example.com", now.Add(-20*day), "udp", 6969, "1.2.3.4")

	// Never probed is not the same as never down. A name with no measurement
	// behind it must not be recommended, though /all still knows about it.
	if got := urls(t, get(t, h, "/api/list/stable")); len(got) != 0 {
		t.Errorf("stable = %v, want nothing: the name has no measured uptime", got)
	}
	if got := urls(t, get(t, h, "/api/list/all")); len(got) != 1 {
		t.Errorf("all = %v, want the unprobed name", got)
	}
}

func TestListMinAge(t *testing.T) {
	h, st := testServer(t)
	now := time.Now().UTC()
	_, fresh := listTracker(t, st, "fresh.example.com", now.Add(-2*day), "udp", 6969, "1.2.3.4")
	putVerdict(t, st, fresh, "1.2.3.4", store.ProbeLive, now.Add(-2*day))

	// Two days of history is not enough to call a tracker stable, so the
	// default ten-day floor drops it. Asking for no floor brings it back, and
	// min_age_days=0 must mean zero rather than restore the default.
	if got := urls(t, get(t, h, "/api/list/stable")); len(got) != 0 {
		t.Errorf("stable = %v, want nothing: the name is two days old", got)
	}
	if got := urls(t, get(t, h, "/api/list/stable?min_age_days=0")); len(got) != 1 {
		t.Errorf("stable with no age floor = %v, want the fresh name", got)
	}
	if got := urls(t, get(t, h, "/api/list/stable?min_age_days=1")); len(got) != 1 {
		t.Errorf("stable with a one-day floor = %v, want the fresh name", got)
	}
	if rec := get(t, h, "/api/list/stable?min_age_days=soon"); rec.Code != http.StatusBadRequest {
		t.Errorf("garbage min_age_days = %d, want 400", rec.Code)
	}
}

func TestListByScheme(t *testing.T) {
	h, st := testServer(t)
	now := time.Now().UTC()
	_, udp := listTracker(t, st, "udp.example.com", now.Add(-20*day), "udp", 6969, "1.2.3.4")
	putVerdict(t, st, udp, "1.2.3.4", store.ProbeLive, now.Add(-20*day))
	_, web := listTracker(t, st, "web.example.com", now.Add(-20*day), "https", 443, "1.2.3.5")
	putVerdict(t, st, web, "1.2.3.5", store.ProbeLive, now.Add(-20*day))

	if got := urls(t, get(t, h, "/api/list/udp")); len(got) != 1 || !strings.HasPrefix(got[0], "udp://") {
		t.Errorf("udp list = %v", got)
	}
	// The http list covers https too: TLS or not is not the transport a caller
	// is asking about.
	if got := urls(t, get(t, h, "/api/list/http")); len(got) != 1 || !strings.HasPrefix(got[0], "https://") {
		t.Errorf("http list = %v", got)
	}
}

func TestListLiveIsPerEndpoint(t *testing.T) {
	h, st := testServer(t)
	ctx := t.Context()
	now := time.Now().UTC()
	trackerID, udp := listTracker(t, st, "half.example.com", now.Add(-20*day), "udp", 6969, "1.2.3.4")
	if _, err := st.AddEndpoint(ctx, trackerID, "https", 443, "/announce", now.Add(-20*day)); err != nil {
		t.Fatal(err)
	}
	putVerdict(t, st, udp, "1.2.3.4", store.ProbeLive, now.Add(-20*day))
	putVerdict(t, st, endpointOf(t, st, trackerID, "https"), "1.2.3.4",
		store.ProbeDead, now.Add(-20*day))

	// The name is up, but on only one of its two announce URLs. A list working
	// per name would hand a client the one that does not answer.
	got := urls(t, get(t, h, "/api/list/live"))
	if len(got) != 1 || got[0] != "udp://half.example.com:6969/announce" {
		t.Errorf("live = %v, want only the endpoint that answers", got)
	}
	if all := urls(t, get(t, h, "/api/list/all")); len(all) != 2 {
		t.Errorf("all = %v, want both endpoints", all)
	}
}

func TestListLiveNeedsAnAnswer(t *testing.T) {
	h, st := testServer(t)
	now := time.Now().UTC()
	_, ep := listTracker(t, st, "quiet.example.com", now.Add(-20*day), "udp", 6969, "1.2.3.4")
	putVerdict(t, st, ep, "1.2.3.4", store.ProbeUnknown, now.Add(-20*day))

	// A probe that could not be made is not an answer, so an endpoint nobody
	// could reach is not live however good its history.
	if got := urls(t, get(t, h, "/api/list/live")); len(got) != 0 {
		t.Errorf("live = %v, want nothing: the last probe measured nothing", got)
	}
}

func TestListFamilyFilters(t *testing.T) {
	h, st := testServer(t)
	now := time.Now().UTC()
	_, v4 := listTracker(t, st, "v4.example.com", now.Add(-20*day), "udp", 6969, "1.2.3.4")
	putVerdict(t, st, v4, "1.2.3.4", store.ProbeLive, now.Add(-20*day))
	_, dual := listTracker(t, st, "dual.example.com", now.Add(-20*day), "udp", 6969, "1.2.3.5", "2001:db8::1")
	putVerdict(t, st, dual, "1.2.3.5", store.ProbeLive, now.Add(-20*day))

	// Dropping the IPv4-only trackers leaves the names that also answer over
	// IPv6, which is what a caller without IPv4 is really asking for.
	got := urls(t, get(t, h, "/api/list/stable?include_ipv4_only_trackers=false"))
	if len(got) != 1 || !strings.Contains(got[0], "dual.example.com") {
		t.Errorf("without v4-only = %v, want the dual-stack name", got)
	}
	if got := urls(t, get(t, h, "/api/list/stable?include_ipv6_only_trackers=0")); len(got) != 2 {
		t.Errorf("without v6-only = %v, want both: neither name is v6-only", got)
	}
	if got := urls(t, get(t, h, "/api/list/stable?include_ipv4_only_trackers=yes")); len(got) != 2 {
		t.Errorf("anything but false or 0 leaves the flag on, got %v", got)
	}
}

func TestListCapsPerAS(t *testing.T) {
	h, st := testServer(t)
	now := time.Now().UTC()
	for _, tc := range []struct{ name, ip string }{
		{"a.example.com", "1.2.3.4"},
		{"b.example.com", "1.2.3.5"},
		{"c.example.com", "9.9.9.9"},
	} {
		_, ep := listTracker(t, st, tc.name, now.Add(-20*day), "udp", 6969, tc.ip)
		putVerdict(t, st, ep, tc.ip, store.ProbeLive, now.Add(-20*day))
	}
	for ip, asn := range map[string]int{"1.2.3.4": 13335, "1.2.3.5": 13335, "9.9.9.9": 24940} {
		if err := st.PutIPInfo(t.Context(), store.IPInfo{
			IP: ip, Family: 4, ASN: asn, ASName: "test", Prefix: ip + "/32",
		}, now); err != nil {
			t.Fatal(err)
		}
	}

	// Three healthy trackers, two of them on one network. A list capped at one
	// per AS keeps a client from putting all its announces behind one operator.
	got := urls(t, get(t, h, "/api/list/stable?per_as=1"))
	if len(got) != 2 {
		t.Fatalf("capped list = %v, want 2 entries", got)
	}
	if !strings.Contains(strings.Join(got, " "), "c.example.com") {
		t.Errorf("capped list = %v, want the tracker on its own AS kept", got)
	}
	if uncapped := urls(t, get(t, h, "/api/list/stable")); len(uncapped) != 3 {
		t.Errorf("uncapped list = %v, want all three", uncapped)
	}
}

func TestListEndpointsOfOneTrackerCountOnce(t *testing.T) {
	h, st := testServer(t)
	ctx := t.Context()
	now := time.Now().UTC()
	trackerID, udp := listTracker(t, st, "both.example.com", now.Add(-20*day), "udp", 6969, "1.2.3.4")
	putVerdict(t, st, udp, "1.2.3.4", store.ProbeLive, now.Add(-20*day))
	if _, err := st.AddEndpoint(ctx, trackerID, "https", 443, "/announce", now.Add(-20*day)); err != nil {
		t.Fatal(err)
	}
	putVerdict(t, st, endpointOf(t, st, trackerID, "https"), "1.2.3.4",
		store.ProbeLive, now.Add(-20*day))
	if err := st.PutIPInfo(ctx, store.IPInfo{
		IP: "1.2.3.4", Family: 4, ASN: 13335, ASName: "test", Prefix: "1.2.3.4/32",
	}, now); err != nil {
		t.Fatal(err)
	}

	// One hostname serving two endpoints is one tracker: both announce URLs
	// belong on the list, and a per-AS cap of one must not split them.
	if got := urls(t, get(t, h, "/api/list/stable")); len(got) != 2 {
		t.Errorf("list = %v, want both endpoints", got)
	}
	if got := urls(t, get(t, h, "/api/list/stable?per_as=1")); len(got) != 2 {
		t.Errorf("capped list = %v, want both endpoints of the one tracker", got)
	}
}

func TestTrackersCarryUptime(t *testing.T) {
	h, st := testServer(t)
	now := seedLists(t, st)
	listTracker(t, st, "quiet.example.com", now.Add(-20*day), "udp", 6969, "1.2.3.6")

	var got []store.TrackerView
	getJSON(t, h, "/api/trackers", &got)

	uptime := map[string]*float64{}
	for _, v := range got {
		uptime[v.Name] = v.Uptime
	}
	if u := uptime["good.example.com"]; u == nil || *u != 1 {
		t.Errorf("good uptime = %v, want 1", u)
	}
	// Fifteen of twenty measured days answering. Rounding is the caller's
	// business; the API reports the share it measured.
	if u := uptime["flaky.example.com"]; u == nil || *u < 0.7 || *u > 0.8 {
		t.Errorf("flaky uptime = %v, want about 0.75", u)
	}
	// Null rather than zero: nothing has ever spoken to this one, and a
	// scoreless name must not read as a dead one.
	if u, ok := uptime["quiet.example.com"]; !ok || u != nil {
		t.Errorf("unprobed uptime = %v, want null", u)
	}
}

func TestTrackersCarryTheCurrentState(t *testing.T) {
	h, st := testServer(t)
	now := seedLists(t, st)
	listTracker(t, st, "quiet.example.com", now.Add(-20*day), "udp", 6969, "1.2.3.6")

	var got []store.TrackerView
	getJSON(t, h, "/api/trackers", &got)

	state := map[string]*store.TrackerState{}
	for _, v := range got {
		state[v.Name] = v.State
	}

	// Answering throughout, so the stretch runs back to the first measurement
	// rather than to the most recent probe.
	good := state["good.example.com"]
	if good == nil || !good.Answering {
		t.Fatalf("good = %+v, want an answering stretch", good)
	}
	if since := now.Sub(good.Since); since < 19*day || since > 21*day {
		t.Errorf("answering for %v, want about 20 days", since)
	}

	// Silence is dated from the last answer, five days ago, not from when the
	// tracker was first probed twenty days ago.
	flaky := state["flaky.example.com"]
	if flaky == nil || flaky.Answering {
		t.Fatalf("flaky = %+v, want a silent stretch", flaky)
	}
	if since := now.Sub(flaky.Since); since < 4*day || since > 6*day {
		t.Errorf("silent for %v, want about 5 days", since)
	}

	// Nothing has ever been measured, so there is no present state to describe.
	if s, ok := state["quiet.example.com"]; !ok || s != nil {
		t.Errorf("unprobed state = %+v, want null", s)
	}
}
