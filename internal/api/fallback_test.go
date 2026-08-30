package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pawal/torrent-tracker/internal/store"
)

// The Accept header is what decides between the two forms, and the shared get
// helper sends none.
func getAs(t *testing.T, h http.Handler, path, accept string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

const (
	browserAccept = "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8"
	lynxAccept    = "text/html, text/plain, text/sgml, */*;q=0.01"
)

// curl, wget and httpie send */* and never name text/html; a browser and lynx
// both name it and get the page they can render.
func TestPageContentNegotiation(t *testing.T) {
	h, st := testServer(t)
	seed(t, st)

	for _, c := range []struct{ name, path, accept, want string }{
		{"curl", "/", "*/*", "text/plain"},
		{"httpie", "/trackers", "application/json, */*", "text/plain"},
		{"browser", "/", browserAccept, "text/html"},
		{"lynx", "/", lynxAccept, "text/html"},
		{"no accept header", "/", "", "text/html"},
		{"asked for text", "/?format=txt", browserAccept, "text/plain"},
		{"asked for text on a detail page", "/t/a.example.com?format=text", browserAccept, "text/plain"},
	} {
		t.Run(c.name, func(t *testing.T) {
			rec := getAs(t, h, c.path, c.accept)
			if rec.Code != http.StatusOK {
				t.Fatalf("GET %s = %d", c.path, rec.Code)
			}
			if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, c.want) {
				t.Errorf("Content-Type = %q, want %s", ct, c.want)
			}
		})
	}
}

// An unrecognised format is not a request for text: ?format=json would
// otherwise hand a caller a page of prose labelled as what they asked for.
func TestWantsTextIgnoresOtherFormats(t *testing.T) {
	for _, c := range []struct {
		query, accept string
		want          bool
	}{
		{"", "*/*", true},
		{"", browserAccept, false},
		{"", "", false},
		{"?format=txt", browserAccept, true},
		{"?format=text", "", true},
		{"?format=json", "*/*", false},
		{"?format=html", "*/*", false},
	} {
		r := httptest.NewRequest(http.MethodGet, "/"+c.query, nil)
		if c.accept != "" {
			r.Header.Set("Accept", c.accept)
		}
		if got := wantsText(r); got != c.want {
			t.Errorf("wantsText(%q, Accept: %q) = %v, want %v", c.query, c.accept, got, c.want)
		}
	}
}

// The point of the whole thing: a client that runs no JS used to get an empty
// div, so every page has to carry its own data.
func TestTextPagesCarryTheirData(t *testing.T) {
	h, st := testServer(t)
	seed(t, st)

	for _, c := range []struct {
		path string
		want []string
	}{
		{"/", []string{"Trackers tracked", "Recent changes", "a.example.com", "+ 1.2.3.4", "nxdomain"}},
		{"/trackers", []string{"Known trackers", "a.example.com", "b.example.com", "nxdomain"}},
		{"/networks", []string{"Tracker reachability", "Enrichment coverage"}},
		{"/t/a.example.com", []string{
			"a.example.com", "Address history", "1.2.3.4", "2001:db8::1", "Change log", "list.txt"}},
	} {
		body := getAs(t, h, c.path, "*/*").Body.String()
		for _, want := range c.want {
			if !strings.Contains(body, want) {
				t.Errorf("GET %s text is missing %q:\n%s", c.path, want, body)
			}
		}
	}
}

// Every page names the others and the endpoints a terminal reader came for,
// since there is no header or footer without the bundle.
func TestTextPagesLinkOnwards(t *testing.T) {
	h, st := testServer(t)
	seed(t, st)

	body := getAs(t, h, "/", "*/*").Body.String()
	for _, want := range []string{"/trackers", "/networks", "/api/list", "/api/trackers", repoURL} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard text does not mention %q:\n%s", want, body)
		}
	}
}

// The HTML form rides inside the shell, beside the div the bundle mounts into
// rather than instead of it.
func TestShellCarriesTheRenderedBody(t *testing.T) {
	h, st := testServer(t)
	seed(t, st)

	body := getAs(t, h, "/t/a.example.com", browserAccept).Body.String()
	for _, want := range []string{
		`<div id="app">`,
		`<div id="fallback">`,
		"<h1>a.example.com</h1>",
		`<a href="/trackers">`,
		"<td>1.2.3.4</td>",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("shell is missing %q:\n%s", want, body)
		}
	}
}

// A shell with no </body> is malformed, but it must not cost the page its
// metadata as well.
func TestShellWithoutBodyStillGetsItsHead(t *testing.T) {
	out := renderShell([]byte("<!doctype html><title>x</title></head>"), head{
		Title: "t", Body: []byte("<div id=\"fallback\">y</div>"),
	})
	if strings.Contains(string(out), "fallback") {
		t.Errorf("body injected with nowhere to put it: %s", out)
	}
	if !strings.Contains(string(out), "<title>") {
		t.Errorf("head lost: %s", out)
	}
}

// A 404 has to say so in both forms, or a text browser reads an empty page as
// a broken server.
func TestFallbackNotFound(t *testing.T) {
	h, st := testServer(t)
	seed(t, st)

	for _, accept := range []string{"*/*", browserAccept} {
		rec := getAs(t, h, "/nope", accept)
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET /nope (Accept: %s) = %d, want 404", accept, rec.Code)
		}
		if body := rec.Body.String(); !strings.Contains(body, "No such page") {
			t.Errorf("404 body does not say so:\n%s", body)
		}
	}
}

// The country filter is a different page, so it has to narrow the rendered
// one and not only the one the bundle draws.
func TestTextTrackerListFiltersByCountry(t *testing.T) {
	h, st := testServer(t)
	seed(t, st)

	err := st.PutIPInfo(t.Context(), store.IPInfo{
		IP: "1.2.3.4", Family: 4, ASN: 64500, ASName: "EXAMPLE-AS", Country: "SE", RIR: "ripe",
	}, base)
	if err != nil {
		t.Fatal(err)
	}

	body := getAs(t, h, "/trackers?country=SE", "*/*").Body.String()
	if !strings.Contains(body, "a.example.com") {
		t.Errorf("SE page dropped the tracker in SE:\n%s", body)
	}
	if strings.Contains(body, "b.example.com") {
		t.Errorf("SE page kept a tracker with no address at all:\n%s", body)
	}
	if !strings.Contains(body, "AS64500 SE") {
		t.Errorf("network column is missing the AS and country:\n%s", body)
	}
}

// The feed is built from stored strings. A name is not supposed to carry
// markup, but nothing stops one, and it lands inside the shell.
func TestFallbackEscapesMarkup(t *testing.T) {
	h, st := testServer(t)
	if _, _, err := st.AddTracker(t.Context(), "<script>alert(1)</script>", "test", base); err != nil {
		t.Fatal(err)
	}

	body := getAs(t, h, "/trackers", browserAccept).Body.String()
	if strings.Contains(body, "<script>alert(1)</script>") {
		t.Errorf("markup in a tracker name reached the page unescaped:\n%s", body)
	}
	if !strings.Contains(body, "&lt;script&gt;alert(1)&lt;/script&gt;") {
		t.Errorf("escaped name is missing:\n%s", body)
	}
}

// Mirrors describe() in web/src/lib/api.js. A type neither of them knows has
// to render as itself rather than vanish.
func TestDescribeChange(t *testing.T) {
	for _, c := range []struct {
		change store.Change
		want   string
	}{
		{store.Change{Type: store.ChangeIPAdded, IP: "1.2.3.4"}, "+ 1.2.3.4"},
		{store.Change{Type: store.ChangeIPRemoved, IP: "1.2.3.4"}, "- 1.2.3.4"},
		{store.Change{Type: store.ChangePrefixAdded, IP: "2001:db8::/48"}, "+ 2001:db8::/48 (prefix)"},
		{store.Change{Type: store.ChangeStatusChanged, Detail: "ok -> nxdomain"}, "! ok -> nxdomain"},
		{store.Change{Type: store.ChangeTrackerAdded}, "* added"},
		{store.Change{Type: store.ChangeTrackerAdded, Detail: "list.txt"}, "* added (list.txt)"},
		{store.Change{Type: store.ChangeIPsRolling, Family: 6, Detail: "8 per run"}, "~ IPv6 rolls: 8 per run"},
		{store.Change{Type: store.ChangeParked}, "! parked"},
		{store.Change{Type: store.ChangeBEP34Added, Detail: "BITTORRENT UDP:1337"}, "~ publishes BITTORRENT UDP:1337"},
		{store.Change{Type: "invented_later", Detail: "why"}, "? invented_later why"},
	} {
		if got := describeChange(c.change); got != c.want {
			t.Errorf("describeChange(%q) = %q, want %q", c.change.Type, got, c.want)
		}
	}
}

// Mirrors describeNetwork() in web/src/lib/api.js: the Cymru handle prefix and
// the trailing country are shown elsewhere and only make the column wider.
func TestDescribeNetwork(t *testing.T) {
	for _, c := range []struct {
		ref  store.NetworkRef
		want string
	}{
		{store.NetworkRef{ASN: 13335, Holder: "CLOUDFLARENET - Cloudflare, Inc., US"}, "AS13335 Cloudflare, Inc."},
		{store.NetworkRef{ASN: 64500}, "AS64500"},
		{store.NetworkRef{Holder: "Someone"}, "Someone"},
		{store.NetworkRef{}, ""},
	} {
		if got := describeNetwork(c.ref); got != c.want {
			t.Errorf("describeNetwork(%+v) = %q, want %q", c.ref, got, c.want)
		}
	}
}

// A terminal has no table layout, so the renderer has to do it: columns line
// up under their heading and no line carries trailing space.
func TestRenderDocTextAlignsColumns(t *testing.T) {
	out := string(renderDocText(doc{
		Title: "Title",
		Sections: []section{{
			Heading: "Rows",
			Table: &table{
				Head: []string{"Name", "Count"},
				Rows: [][]cell{
					{link("a-very-long-name", "/t/a"), txt("1")},
					{txt("short"), txt("22")},
				},
			},
		}},
	}))

	lines := strings.Split(out, "\n")
	var body []string
	for _, l := range lines {
		if strings.TrimSpace(l) != "" {
			body = append(body, l)
		}
		if l != strings.TrimRight(l, " ") {
			t.Errorf("trailing space on %q", l)
		}
	}
	// Title, its rule, the heading, its rule, the head row, the column rules
	// and two rows.
	if len(body) != 8 {
		t.Fatalf("got %d lines, want 8:\n%s", len(body), out)
	}
	if !strings.Contains(out, "Title\n=====\n") {
		t.Errorf("title is not underlined:\n%s", out)
	}
	col := strings.Index(body[4], "Count")
	for _, l := range body[6:] {
		if got := strings.Index(l, strings.Fields(l)[1]); got != col {
			t.Errorf("second column at %d, want %d: %q", got, col, l)
		}
	}
	// A terminal cannot follow a link, so a table cell is its text alone.
	if strings.Contains(out, "/t/a") {
		t.Errorf("href leaked into a table cell:\n%s", out)
	}
}

// The nav and footer are the only way out of a text page, so those hrefs do
// have to be spelled out.
func TestRenderDocTextSpellsOutNavLinks(t *testing.T) {
	out := string(renderDocText(doc{
		Title:  "Title",
		Nav:    []cell{txt("Changes"), link("Trackers", "/trackers")},
		Footer: []cell{link("JSON API", "/api/trackers")},
	}))
	if !strings.Contains(out, "Trackers (/trackers)") {
		t.Errorf("nav link not spelled out:\n%s", out)
	}
	if !strings.Contains(out, "JSON API (/api/trackers)") {
		t.Errorf("footer link not spelled out:\n%s", out)
	}
}
