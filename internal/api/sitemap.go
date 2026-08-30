package api

import (
	"encoding/xml"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// baseURL is the origin absolute links are built from: BaseURL if set, else
// what the request and any proxy headers say.
func (s *Server) baseURL(r *http.Request) string {
	if s.BaseURL != "" {
		return strings.TrimSuffix(s.BaseURL, "/")
	}

	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if p, _, _ := strings.Cut(r.Header.Get("X-Forwarded-Proto"), ","); p != "" {
		if p = strings.TrimSpace(p); p == "http" || p == "https" {
			scheme = p
		}
	}
	host := r.Host
	if h, _, _ := strings.Cut(r.Header.Get("X-Forwarded-Host"), ","); h != "" {
		host = strings.TrimSpace(h)
	}
	return scheme + "://" + host
}

// handleRobots names the sitemap and keeps crawlers off the API.
func (s *Server) handleRobots(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	fmt.Fprintf(w, "User-agent: *\nAllow: /\nDisallow: /api/\nDisallow: /healthz\n\nSitemap: %s/sitemap.xml\n",
		s.baseURL(r))
}

type sitemapURL struct {
	Loc     string `xml:"loc"`
	LastMod string `xml:"lastmod,omitempty"`
}

type urlset struct {
	XMLName xml.Name     `xml:"urlset"`
	NS      string       `xml:"xmlns,attr"`
	URLs    []sitemapURL `xml:"url"`
}

// handleSitemap lists the three top pages and one per live tracker. Country
// views are subsets, reachable by link, and left out.
func (s *Server) handleSitemap(w http.ResponseWriter, r *http.Request) {
	trackers, err := s.Store.ListTrackers(r.Context(), false)
	if err != nil {
		s.serverError(w, err)
		return
	}
	changed, err := s.Store.LastChangePerTracker(r.Context())
	if err != nil {
		s.serverError(w, err)
		return
	}

	base := s.baseURL(r)
	var newest time.Time
	for _, at := range changed {
		if at.After(newest) {
			newest = at
		}
	}

	set := urlset{NS: "http://www.sitemaps.org/schemas/sitemap/0.9"}
	for _, p := range []string{pathDashboard, pathTrackers, pathNetworks} {
		set.URLs = append(set.URLs, sitemapURL{Loc: base + p, LastMod: isoDate(newest)})
	}
	for _, t := range trackers {
		// A check is not a change: dating by the last poll would rewrite every
		// entry hourly.
		at, ok := changed[t.ID]
		if !ok {
			at = t.CreatedAt
		}
		set.URLs = append(set.URLs, sitemapURL{
			Loc:     base + trackerPrefix + url.PathEscape(t.Name),
			LastMod: isoDate(at),
		})
	}

	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	w.Write([]byte(xml.Header))
	enc := xml.NewEncoder(w)
	enc.Indent("", "  ")
	if err := enc.Encode(set); err != nil {
		s.logger().Error("encode sitemap", "err", err)
	}
	w.Write([]byte("\n"))
}

// isoDate is the W3C date a sitemap wants, at day granularity.
func isoDate(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format("2006-01-02")
}
