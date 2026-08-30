package api

import (
	"io/fs"
	"net/http"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/pawal/torrent-tracker/internal/store"
	"github.com/pawal/torrent-tracker/web"
)

// renderShell works by substituting the strings web/index.html ships with. If
// that file is edited without updating these constants the substitution
// silently stops happening, and every page goes out describing the dashboard.
func TestShellCarriesThePlaceholders(t *testing.T) {
	shell := string(realShell(t))
	for _, want := range []string{defaultTitle, defaultDesc, defaultURL, defaultImage, robotsIndex} {
		if !strings.Contains(shell, want) {
			t.Errorf("the built index.html no longer contains %q", want)
		}
	}
}

// realShell is the shell the binary actually ships, as opposed to the stub in
// testServer. The substitution is only meaningful against the real one.
func realShell(t *testing.T) []byte {
	t.Helper()
	dist, err := web.Dist()
	if err != nil {
		t.Fatal(err)
	}
	b, err := fs.ReadFile(dist, "index.html")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// realServer serves the built shell, so a test can assert on the bytes a
// crawler receives.
func realServer(t *testing.T) (http.Handler, *store.Store) {
	t.Helper()
	st := testStore(t)
	srv := &Server{
		Store:  st,
		Log:    discard(),
		Static: fstest.MapFS{"index.html": &fstest.MapFile{Data: realShell(t)}},
	}
	return srv.Handler(), st
}

// The metadata has to reach the shell for every tag, not just <title>: an
// unfurler reads og:*, and a search engine reads the description.
func TestServedPageCarriesItsOwnMetadata(t *testing.T) {
	body := string(renderShell(realShell(t), head{
		Title:       "a.example.com — torrent-tracker",
		Description: "a description",
		URL:         "https://tracker.example.com/t/a.example.com",
		Image:       "https://tracker.example.com/og-image.png",
	}))

	for _, want := range []string{
		"<title>a.example.com — torrent-tracker</title>",
		`property="og:title" content="a.example.com — torrent-tracker"`,
		`name="twitter:title" content="a.example.com — torrent-tracker"`,
		`name="description" content="a description"`,
		`property="og:description" content="a description"`,
		`rel="canonical" href="https://tracker.example.com/t/a.example.com"`,
		`property="og:url" content="https://tracker.example.com/t/a.example.com"`,
		`property="og:image" content="https://tracker.example.com/og-image.png"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the served page is missing %s", want)
		}
	}
	// The image URL contains the page URL as a prefix; replacing in the wrong
	// order corrupts it into ".../t/a.example.comog-image.png".
	if strings.Contains(body, "comog-image") {
		t.Error("the image URL was mangled by the page URL substitution")
	}
}

// End to end: the response a crawler actually gets.
func TestTrackerPageIsDescribed(t *testing.T) {
	h, st := realServer(t)
	seed(t, st)

	rec := get(t, h, "/t/a.example.com")
	body := rec.Body.String()
	if !strings.Contains(body, "<title>a.example.com — torrent-tracker</title>") {
		t.Errorf("the tracker page does not name itself:\n%s", body)
	}
	if !strings.Contains(body, "http://example.com/t/a.example.com") {
		t.Errorf("the canonical URL is not this page:\n%s", body)
	}
}

// A 404 must not be indexed, or the typos become pages in their own right.
func TestNotFoundIsNoIndex(t *testing.T) {
	h, st := realServer(t)
	seed(t, st)

	for _, path := range []string{"/nope", "/t/ghost.example.com"} {
		body := get(t, h, path).Body.String()
		if !strings.Contains(body, robotsNone) {
			t.Errorf("GET %s is not marked noindex", path)
		}
		if !strings.Contains(body, "<title>Not found — torrent-tracker</title>") {
			t.Errorf("GET %s does not say it is a 404", path)
		}
		// Naming a canonical URL for a page that does not exist contradicts
		// the noindex beside it, so the tag goes rather than the URL changing.
		if strings.Contains(body, `rel="canonical"`) {
			t.Errorf("GET %s still declares a canonical URL", path)
		}
	}
	// And a real page keeps the opposite.
	if body := get(t, h, "/trackers").Body.String(); !strings.Contains(body, robotsIndex) {
		t.Error("a real page was marked noindex")
	}
}

// The one query parameter that makes a different page is kept; anything else
// would let a tracking tag mint an endless supply of duplicates.
func TestCanonicalKeepsOnlyTheCountryFilter(t *testing.T) {
	h, st := realServer(t)
	seed(t, st)

	body := get(t, h, "/trackers?country=SE&utm_source=spam").Body.String()
	if !strings.Contains(body, `rel="canonical" href="http://example.com/trackers?country=SE"`) {
		t.Errorf("the canonical URL is wrong:\n%s", head100(body))
	}
	if strings.Contains(body, "utm_source") {
		t.Error("a tracking parameter reached the canonical URL")
	}
	if !strings.Contains(body, "<title>Trackers in SE — torrent-tracker</title>") {
		t.Error("the filtered list did not get its own title")
	}
	// The filter means nothing on the other pages, so it is not carried there.
	if body := get(t, h, "/networks?country=SE").Body.String(); strings.Contains(body, "country=SE") {
		t.Error("the country filter leaked onto the networks page")
	}
}

// A name is data, and reaches an attribute and an element. Both need escaping.
func TestMetadataIsEscaped(t *testing.T) {
	body := string(renderShell(realShell(t), head{
		Title:       `a"><script>alert(1)</script>.example.com`,
		Description: "x & y",
		URL:         "http://example.com/",
		Image:       "http://example.com/og-image.png",
	}))
	if strings.Contains(body, "<script>alert(1)</script>") {
		t.Error("a name was written into the head unescaped")
	}
	if !strings.Contains(body, "x &amp; y") {
		t.Error("an ampersand was not escaped")
	}
}

func TestPageMeta(t *testing.T) {
	tests := []struct {
		name, path, country string
		tracker             *store.Tracker
		wantTitle           string
	}{
		{name: "dashboard", path: "/", wantTitle: defaultTitle},
		{name: "list", path: "/trackers", wantTitle: "Known trackers — torrent-tracker"},
		{name: "filtered", path: "/trackers", country: "SE", wantTitle: "Trackers in SE — torrent-tracker"},
		{
			name: "no country", path: "/trackers", country: "unknown",
			wantTitle: "Trackers with no country on record — torrent-tracker",
		},
		{name: "networks", path: "/networks", wantTitle: "Networks — torrent-tracker"},
		{
			name: "detail", path: "/t/a.example.com",
			tracker:   &store.Tracker{Name: "a.example.com", Reach: store.ReachLive},
			wantTitle: "a.example.com — torrent-tracker",
		},
		{name: "unknown", path: "/nope", wantTitle: "Not found — torrent-tracker"},
	}

	seen := map[string]string{}
	for _, tt := range tests {
		title, desc := pageMeta(tt.path, tt.country, tt.tracker)
		if title != tt.wantTitle {
			t.Errorf("%s: title = %q, want %q", tt.name, title, tt.wantTitle)
		}
		if desc == "" {
			t.Errorf("%s: no description", tt.name)
		}
		// Two pages with one description are two pages a search engine cannot
		// tell apart, which is what this whole change is about.
		if other, dup := seen[desc]; dup {
			t.Errorf("%s and %s share a description", tt.name, other)
		}
		seen[desc] = tt.name
	}
}

// The sentence has to follow the tracker's actual state, and the JS mirror in
// web/src/lib/meta.js has to produce the same one for the same row.
func TestTrackerState(t *testing.T) {
	tests := []struct {
		name string
		in   store.Tracker
		want string
	}{
		{"live", store.Tracker{Reach: store.ReachLive}, "resolves and answers the tracker protocol"},
		{"partial", store.Tracker{Reach: store.ReachPartial}, "answers on some of its addresses"},
		{
			"dead but resolving",
			store.Tracker{Reach: store.ReachDead, LastStatus: store.StatusOK},
			"resolves but answers nothing",
		},
		{
			"gone from DNS",
			store.Tracker{Reach: store.ReachDead, LastStatus: store.StatusNXDomain},
			"does not resolve (nxdomain)",
		},
		{"unprobed", store.Tracker{LastStatus: store.StatusOK}, "has not been probed yet"},
		{
			// Parking beats reachability: whatever answers is not the tracker.
			"parked",
			store.Tracker{Parked: true, Reach: store.ReachLive},
			"resolves only to parking addresses",
		},
		{
			// And an operator asking not to be contacted beats everything.
			"denies",
			store.Tracker{BEP34Denies: true, Parked: true, Reach: store.ReachLive},
			"publishes a BEP 34 record naming no tracker and is no longer probed",
		},
	}
	for _, tt := range tests {
		if got := trackerState(tt.in); got != tt.want {
			t.Errorf("%s: trackerState = %q, want %q", tt.name, got, tt.want)
		}
	}
}

func head100(s string) string {
	if len(s) > 900 {
		return s[:900]
	}
	return s
}
