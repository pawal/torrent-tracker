package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/pawal/torrent-tracker/internal/store"
)

var base = time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)

// testStore opens an empty database that goes away with the test.
func testStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "api.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// discard keeps the request log quiet; no test asserts on it.
func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func testServer(t *testing.T) (http.Handler, *store.Store) {
	t.Helper()
	st := testStore(t)
	srv := &Server{
		Store: st,
		Log:   discard(),
		Static: fstest.MapFS{
			"index.html":    &fstest.MapFile{Data: []byte("<!doctype html><title>app</title>")},
			"assets/app.js": &fstest.MapFile{Data: []byte("console.log(1)")},
		},
	}
	return srv.Handler(), st
}

// seed puts one healthy tracker and one dead one into the store.
func seed(t *testing.T, st *store.Store) {
	t.Helper()
	ctx := t.Context()

	a, _, err := st.AddTracker(ctx, "a.example.com", "list.txt", base)
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := st.AddTracker(ctx, "b.example.com", "manual", base)
	if err != nil {
		t.Fatal(err)
	}

	if err := st.ApplyPlan(ctx, a.ID, store.Plan{
		Status: store.StatusOK, StatusChanged: true,
		Actions: []store.Action{
			{IP: "1.2.3.4", Family: 4, Kind: store.ActionAdd},
			{IP: "2001:db8::1", Family: 6, Kind: store.ActionAdd},
		},
	}, base); err != nil {
		t.Fatal(err)
	}
	if err := st.ApplyPlan(ctx, b.ID, store.Plan{
		Status: store.StatusNXDomain, StatusChanged: true,
	}, base.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
}

func get(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func getJSON(t *testing.T, h http.Handler, path string, out any) {
	t.Helper()
	rec := get(t, h, path)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200 (body: %s)", path, rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Errorf("GET %s Content-Type = %q", path, ct)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
		t.Fatalf("GET %s: decode: %v (body: %s)", path, err, rec.Body.String())
	}
}

func TestHealthz(t *testing.T) {
	h, _ := testServer(t)
	rec := get(t, h, "/healthz")
	if rec.Code != http.StatusOK || rec.Body.String() != "ok\n" {
		t.Errorf("healthz = %d %q", rec.Code, rec.Body.String())
	}
}

func TestGetTrackers(t *testing.T) {
	h, st := testServer(t)
	seed(t, st)

	var got []store.TrackerView
	getJSON(t, h, "/api/trackers", &got)

	if len(got) != 2 {
		t.Fatalf("got %d trackers, want 2", len(got))
	}
	if got[0].Name != "a.example.com" {
		t.Errorf("not sorted by name: %q first", got[0].Name)
	}
	if len(got[0].IPv4) != 1 || got[0].IPv4[0] != "1.2.3.4" {
		t.Errorf("ipv4 = %v", got[0].IPv4)
	}
	if len(got[0].IPv6) != 1 || got[0].IPv6[0] != "2001:db8::1" {
		t.Errorf("ipv6 = %v", got[0].IPv6)
	}
	if got[1].LastStatus != store.StatusNXDomain {
		t.Errorf("b last_status = %q", got[1].LastStatus)
	}
}

// Empty address lists must serialise as [] so the frontend can map over them.
func TestGetTrackersEmptyListsAreArrays(t *testing.T) {
	h, st := testServer(t)
	if _, _, err := st.AddTracker(t.Context(), "bare.example.com", "test", base); err != nil {
		t.Fatal(err)
	}

	rec := get(t, h, "/api/trackers")
	var raw []map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"ipv4", "ipv6"} {
		if string(raw[0][field]) != "[]" {
			t.Errorf("%s = %s, want []", field, raw[0][field])
		}
	}
}

func TestGetTrackersEmptyStore(t *testing.T) {
	h, _ := testServer(t)
	rec := get(t, h, "/api/trackers")
	if got := rec.Body.String(); got != "[]\n" {
		t.Errorf("body = %q, want an empty JSON array", got)
	}
}

func TestGetTrackerDetail(t *testing.T) {
	h, st := testServer(t)
	seed(t, st)

	var got struct {
		store.Tracker
		Records []store.IPRecord `json:"records"`
		Changes []store.Change   `json:"changes"`
	}
	getJSON(t, h, "/api/trackers/a.example.com", &got)

	if got.Name != "a.example.com" || got.Source != "list.txt" {
		t.Errorf("tracker = %+v", got.Tracker)
	}
	if len(got.Records) != 2 {
		t.Errorf("got %d records, want 2", len(got.Records))
	}
	// tracker_added, status_changed, two ip_added.
	if len(got.Changes) != 4 {
		t.Errorf("got %d changes, want 4", len(got.Changes))
	}
	for _, c := range got.Changes {
		if c.Tracker != "a.example.com" {
			t.Errorf("change missing tracker name: %+v", c)
		}
	}
}

// The detail page draws a fixed window, so the payload has to say where that
// window starts and carry the intervals that fall inside it.
func TestGetTrackerDetailProbeHistory(t *testing.T) {
	h, st := testServer(t)
	seed(t, st)
	ctx := t.Context()

	tr, err := st.TrackerByName(ctx, "a.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddEndpoint(ctx, tr.ID, "udp", 6969, "/announce", base); err != nil {
		t.Fatal(err)
	}
	udp := endpointOf(t, st, tr.ID, "udp")

	// Live for a day, then dead: the first verdict closes and only the second
	// is still open.
	now := time.Now().UTC()
	putVerdict(t, st, udp, "1.2.3.4", store.ProbeLive, now.AddDate(0, 0, -2))
	putVerdict(t, st, udp, "1.2.3.4", store.ProbeDead, now.AddDate(0, 0, -1))

	// The window slides with the wall clock while the seeded lookup is anchored
	// to base, so log one inside the window the request actually asks about.
	if err := st.ApplyPlan(ctx, tr.ID, store.Plan{
		Status: store.StatusOK, Duration: 12 * time.Millisecond,
	}, now.AddDate(0, 0, -1)); err != nil {
		t.Fatal(err)
	}

	var got struct {
		History     []store.ProbeInterval  `json:"probe_history"`
		HistoryFrom time.Time              `json:"probe_history_from"`
		Probes      []store.Probe          `json:"probes"`
		Resolution  []store.StatusInterval `json:"resolution"`
		Latency     store.ResolutionStats  `json:"resolution_stats"`
	}
	getJSON(t, h, "/api/trackers/a.example.com?days=7", &got)

	if len(got.History) != 1 || got.History[0].Result != store.ProbeLive {
		t.Fatalf("probe_history = %+v, want one live interval", got.History)
	}
	if len(got.Probes) != 1 || got.Probes[0].Result != store.ProbeDead {
		t.Errorf("probes = %+v, want one dead probe", got.Probes)
	}
	if d := now.Sub(got.HistoryFrom).Hours(); d < 167 || d > 169 {
		t.Errorf("probe_history_from is %.0fh back, want about 168", d)
	}

	// The DNS axis rides the same window, drawn from the lookup log the
	// collector was already writing and nothing was reading.
	if len(got.Resolution) == 0 {
		t.Error("resolution history is empty, want the seeded lookup")
	}
	if got.Latency.Lookups == 0 {
		t.Error("resolution_stats counted no lookups")
	}

	// A nonsense window falls back to the default rather than collapsing to
	// nothing, so the card is never blank because of a bad URL.
	getJSON(t, h, "/api/trackers/a.example.com?days=0", &got)
	if len(got.History) != 1 {
		t.Errorf("days=0 should fall back to the default window, got %+v", got.History)
	}
}

func TestGetTrackerNotFound(t *testing.T) {
	h, st := testServer(t)
	seed(t, st)

	rec := get(t, h, "/api/trackers/nope.example.com")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["error"] == "" {
		t.Error("404 body should carry an error message")
	}
}

func TestGetChanges(t *testing.T) {
	h, st := testServer(t)
	seed(t, st)

	var got []store.Change
	getJSON(t, h, "/api/changes", &got)
	if len(got) != 6 { // 2 tracker_added + 2 status_changed + 2 ip_added
		t.Fatalf("got %d changes, want 6", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i-1].ObservedAt.Before(got[i].ObservedAt) {
			t.Fatalf("changes are not newest-first at index %d", i)
		}
	}

	var limited []store.Change
	getJSON(t, h, "/api/changes?limit=2", &limited)
	if len(limited) != 2 {
		t.Errorf("limit=2 returned %d", len(limited))
	}
}

func TestGetChangesSince(t *testing.T) {
	h, st := testServer(t)
	seed(t, st)

	var got []store.Change
	getJSON(t, h, "/api/changes?since="+base.Add(30*time.Minute).Format(time.RFC3339), &got)
	// Only the second tracker's status change happened after the cutoff.
	if len(got) != 1 {
		t.Errorf("got %d changes after the cutoff, want 1: %+v", len(got), got)
	}
}

func TestGetChangesBadSince(t *testing.T) {
	h, _ := testServer(t)
	rec := get(t, h, "/api/changes?since=yesterday")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// A junk limit should fall back to the default rather than erroring.
func TestGetChangesIgnoresBadLimit(t *testing.T) {
	h, st := testServer(t)
	seed(t, st)
	for _, q := range []string{"limit=abc", "limit=0", "limit=-5"} {
		var got []store.Change
		getJSON(t, h, "/api/changes?"+q, &got)
		if len(got) != 6 {
			t.Errorf("%s returned %d changes, want the default 6", q, len(got))
		}
	}
}

func TestGetStats(t *testing.T) {
	h, st := testServer(t)
	seed(t, st)

	var got store.Stats
	getJSON(t, h, "/api/stats", &got)
	if got.Trackers != 2 || got.EnabledTrackers != 2 {
		t.Errorf("trackers = %d/%d, want 2/2", got.EnabledTrackers, got.Trackers)
	}
	if got.ActiveIPs != 2 {
		t.Errorf("active_ips = %d, want 2", got.ActiveIPs)
	}
	if got.ByStatus[store.StatusOK] != 1 || got.ByStatus[store.StatusNXDomain] != 1 {
		t.Errorf("by_status = %v", got.ByStatus)
	}
}

func TestGetRuns(t *testing.T) {
	h, st := testServer(t)
	ctx := t.Context()

	id, err := st.StartRun(ctx, base, 5)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.FinishRun(ctx, id, base.Add(time.Minute), 4, 1, 3); err != nil {
		t.Fatal(err)
	}

	var got []store.Run
	getJSON(t, h, "/api/runs", &got)
	if len(got) != 1 || got[0].OKCount != 4 || got[0].ErrorCount != 1 {
		t.Errorf("runs = %+v", got)
	}
}

func TestMutatingMethodsAreRejected(t *testing.T) {
	// The API is read-only; the CLI owns mutations, so nothing here needs auth.
	h, st := testServer(t)
	seed(t, st)

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(method, "/api/trackers", nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s /api/trackers = %d, want 405", method, rec.Code)
		}
	}
}

func TestCORSAllowsAnyOrigin(t *testing.T) {
	h, st := testServer(t)
	seed(t, st)

	rec := get(t, h, "/api/stats")
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("Access-Control-Allow-Origin = %q, want %q", got, "*")
	}
	// The frontend is same-origin; only the API is meant to be cross-origin.
	if rec := get(t, h, "/healthz"); rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Error("/healthz should not carry CORS headers")
	}
}

func TestCORSPreflight(t *testing.T) {
	h, _ := testServer(t)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodOptions, "/api/changes", nil)
	req.Header.Set("Origin", "https://example.com")
	req.Header.Set("Access-Control-Request-Method", "GET")
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("Access-Control-Allow-Origin = %q, want %q", got, "*")
	}
	if got := rec.Header().Get("Access-Control-Allow-Methods"); !strings.Contains(got, "GET") {
		t.Errorf("Access-Control-Allow-Methods = %q, want GET in it", got)
	}
}

func TestStaticFrontend(t *testing.T) {
	h, _ := testServer(t)

	rec := get(t, h, "/")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / = %d", rec.Code)
	}
	if body := rec.Body.String(); body == "" {
		t.Error("index.html was not served")
	}

	rec = get(t, h, "/assets/app.js")
	if rec.Code != http.StatusOK || rec.Body.String() != "console.log(1)" {
		t.Errorf("asset = %d %q", rec.Code, rec.Body.String())
	}
}

// Client-side routes must fall back to index.html so deep links work.
func TestStaticSPAFallback(t *testing.T) {
	h, _ := testServer(t)
	rec := get(t, h, "/t/some.tracker.example.com")
	if rec.Code != http.StatusOK {
		t.Fatalf("deep link = %d, want 200 via the index fallback", rec.Code)
	}
	if body := rec.Body.String(); body != "<!doctype html><title>app</title>" {
		t.Errorf("body = %q, want index.html", body)
	}
}

// An unknown /api path must 404 rather than fall through to the SPA, so that
// a typo in the frontend fails loudly instead of returning HTML.
func TestUnknownAPIPathDoesNotServeHTML(t *testing.T) {
	h, _ := testServer(t)
	rec := get(t, h, "/api/nonexistent")

	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET /api/nonexistent = %d, want 404 (body: %s)", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Errorf("Content-Type = %q, want JSON rather than the SPA's HTML", ct)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	if body["error"] == "" {
		t.Error("404 body should carry an error message")
	}
}

func TestServerWithoutStatic(t *testing.T) {
	h := (&Server{Store: testStore(t), Log: discard()}).Handler()
	if rec := get(t, h, "/api/stats"); rec.Code != http.StatusOK {
		t.Errorf("API should work without an embedded frontend: %d", rec.Code)
	}
	if rec := get(t, h, "/"); rec.Code != http.StatusNotFound {
		t.Errorf("GET / = %d, want 404 with no frontend configured", rec.Code)
	}
}

func TestIntParam(t *testing.T) {
	tests := []struct {
		query string
		want  int
	}{
		{"", 50},
		{"?limit=10", 10},
		{"?limit=0", 50},
		{"?limit=-1", 50},
		{"?limit=abc", 50},
		{"?limit=1000", maxLimit},
		{"?limit=999999", maxLimit},
	}
	for _, tt := range tests {
		r := httptest.NewRequest(http.MethodGet, "/x"+tt.query, nil)
		if got := intParam(r, "limit", 50); got != tt.want {
			t.Errorf("intParam(%q) = %d, want %d", tt.query, got, tt.want)
		}
	}
}
