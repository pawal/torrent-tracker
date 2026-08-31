package api

import (
	"crypto/tls"
	"encoding/xml"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// baseURL decides every absolute link the site emits. Getting it wrong points
// canonical tags and the sitemap at a host that is not this one.
func TestBaseURL(t *testing.T) {
	tests := []struct {
		name    string
		config  string
		host    string
		tls     bool
		headers map[string]string
		want    string
	}{
		{name: "from the request", host: "tracker.example.com", want: "http://tracker.example.com"},
		{name: "TLS implies https", host: "tracker.example.com", tls: true, want: "https://tracker.example.com"},
		{
			name: "a proxy says so", host: "127.0.0.1:8080",
			headers: map[string]string{"X-Forwarded-Proto": "https", "X-Forwarded-Host": "tracker.example.com"},
			want:    "https://tracker.example.com",
		},
		{
			// A chain of hops: the first is the client's.
			name: "a chain of proxies", host: "127.0.0.1:8080",
			headers: map[string]string{"X-Forwarded-Proto": "https, http"},
			want:    "https://127.0.0.1:8080",
		},
		{
			// Not a scheme, so it is ignored rather than pasted into a URL.
			name: "junk in the header", host: "tracker.example.com",
			headers: map[string]string{"X-Forwarded-Proto": "gopher"},
			want:    "http://tracker.example.com",
		},
		{
			name: "configuration wins", config: "https://tracker.evilbit.de", host: "127.0.0.1:8080",
			want: "https://tracker.evilbit.de",
		},
		{
			// A trailing slash would double up against the paths appended to it.
			name: "a configured trailing slash is dropped", config: "https://tracker.evilbit.de/",
			host: "127.0.0.1:8080", want: "https://tracker.evilbit.de",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/robots.txt", nil)
			r.Host = tt.host
			if tt.tls {
				r.TLS = &tls.ConnectionState{}
			}
			for k, v := range tt.headers {
				r.Header.Set(k, v)
			}
			s := &Server{BaseURL: tt.config}
			if got := s.baseURL(r); got != tt.want {
				t.Errorf("baseURL = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRobots(t *testing.T) {
	h, _ := testServer(t)
	rec := get(t, h, "/robots.txt")

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /robots.txt = %d", rec.Code)
	}
	// It used to fall through to the SPA, so a crawler got HTML and a 200.
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain", ct)
	}
	body := rec.Body.String()
	for _, want := range []string{"User-agent: *", "Disallow: /api/", "Sitemap: http://example.com/sitemap.xml"} {
		if !strings.Contains(body, want) {
			t.Errorf("robots.txt is missing %q:\n%s", want, body)
		}
	}
}

// The sitemap is the only way a crawler learns the tracker pages exist: they
// are reachable from the list page, but only after JS has run.
func TestSitemapListsEveryTracker(t *testing.T) {
	h, st := testServer(t)
	seed(t, st)

	rec := get(t, h, "/sitemap.xml")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /sitemap.xml = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/xml") {
		t.Errorf("Content-Type = %q", ct)
	}

	var set urlset
	if err := xml.Unmarshal(rec.Body.Bytes(), &set); err != nil {
		t.Fatalf("sitemap is not valid XML: %v\n%s", err, rec.Body.String())
	}

	locs := map[string]string{}
	for _, u := range set.URLs {
		locs[u.Loc] = u.LastMod
	}
	want := []string{
		"http://example.com/",
		"http://example.com/trackers",
		"http://example.com/networks",
		"http://example.com/lists",
		"http://example.com/t/a.example.com",
		"http://example.com/t/b.example.com",
	}
	for _, w := range want {
		if _, ok := locs[w]; !ok {
			t.Errorf("sitemap is missing %s (has %v)", w, set.URLs)
		}
	}
	if len(set.URLs) != len(want) {
		t.Errorf("sitemap has %d URLs, want %d", len(set.URLs), len(want))
	}
}

// lastmod is a date, not a timestamp: an hourly poll must not rewrite every
// entry, or the whole file stops being believed.
func TestSitemapLastModIsADate(t *testing.T) {
	h, st := testServer(t)
	seed(t, st)

	var set urlset
	if err := xml.Unmarshal(get(t, h, "/sitemap.xml").Body.Bytes(), &set); err != nil {
		t.Fatal(err)
	}
	for _, u := range set.URLs {
		if u.LastMod == "" {
			t.Errorf("%s has no lastmod", u.Loc)
			continue
		}
		if _, err := time.Parse("2006-01-02", u.LastMod); err != nil {
			t.Errorf("%s lastmod = %q, want a W3C date", u.Loc, u.LastMod)
		}
	}
}

// A retired name keeps its page but stops being pushed at crawlers.
func TestSitemapOmitsRemovedTrackers(t *testing.T) {
	h, st := testServer(t)
	seed(t, st)

	if err := st.RemoveTracker(t.Context(), "b.example.com", false); err != nil {
		t.Fatal(err)
	}
	body := get(t, h, "/sitemap.xml").Body.String()
	if strings.Contains(body, "b.example.com") {
		t.Errorf("sitemap still lists the removed tracker:\n%s", body)
	}
	if !strings.Contains(body, "a.example.com") {
		t.Error("sitemap dropped the live tracker too")
	}
}

// A name with a character that needs escaping must survive both the URL and
// the XML, or the entry is silently unusable.
func TestSitemapEscapesNames(t *testing.T) {
	h, st := testServer(t)
	if _, _, err := st.AddTracker(t.Context(), "odd name&co.example.com", "manual", base); err != nil {
		t.Fatal(err)
	}

	rec := get(t, h, "/sitemap.xml")
	var set urlset
	if err := xml.Unmarshal(rec.Body.Bytes(), &set); err != nil {
		t.Fatalf("sitemap is not valid XML: %v\n%s", err, rec.Body.String())
	}
	found := false
	for _, u := range set.URLs {
		if strings.Contains(u.Loc, "odd") {
			found = true
			if strings.Contains(u.Loc, " ") {
				t.Errorf("loc has a raw space: %q", u.Loc)
			}
		}
	}
	if !found {
		t.Errorf("the odd name is missing from %v", set.URLs)
	}
}

// Without a frontend there are no pages, so there is nothing to describe.
func TestNoSitemapWithoutStatic(t *testing.T) {
	h := (&Server{Store: testStore(t), Log: discard()}).Handler()
	for _, path := range []string{"/robots.txt", "/sitemap.xml"} {
		if rec := get(t, h, path); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404 with no frontend", path, rec.Code)
		}
	}
}

func TestISODate(t *testing.T) {
	if got := isoDate(time.Date(2026, 8, 30, 23, 59, 0, 0, time.UTC)); got != "2026-08-30" {
		t.Errorf("isoDate = %q", got)
	}
	if got := isoDate(time.Time{}); got != "" {
		t.Errorf("the zero time should yield no lastmod, got %q", got)
	}
}
