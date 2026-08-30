package api

import (
	"context"
	"errors"
	"io/fs"
	"net/http"
	"net/url"
	"strings"

	"github.com/pawal/torrent-tracker/internal/store"
)

// The paths the frontend renders. Anything else gets a 404 rather than the
// shell with a 200, which would have crawlers indexing every typo.
const (
	pathDashboard = "/"
	pathTrackers  = "/trackers"
	pathNetworks  = "/networks"
	trackerPrefix = "/t/"
)

// canonicalPath strips a trailing slash: "/trackers/" and "/trackers" are one
// page, and only one of them should ever be indexed.
func canonicalPath(p string) string {
	if len(p) > 1 && strings.HasSuffix(p, "/") {
		if c := strings.TrimRight(p, "/"); c != "" {
			return c
		}
		return "/"
	}
	return p
}

// docRoute reports whether p is a page, and the tracker name when it is a
// detail page. It mirrors parseRoute in web/src/lib/router.js.
func docRoute(p string) (name string, ok bool) {
	switch p {
	case pathDashboard, pathTrackers, pathNetworks:
		return "", true
	}
	rest, found := strings.CutPrefix(p, trackerPrefix)
	if !found || rest == "" || strings.Contains(rest, "/") {
		return "", false
	}
	return rest, true
}

// shell is index.html, read once. A missing one is a build fault, not a
// request fault, so it is reported as a server error.
func (s *Server) shell() ([]byte, error) {
	s.shellOnce.Do(func() {
		s.shellHTML, s.shellErr = fs.ReadFile(s.Static, "index.html")
	})
	return s.shellHTML, s.shellErr
}

// servePage writes the page: plain text where the client asked for anything
// but HTML, else the shell carrying this page's metadata and a rendered body,
// so a crawler or a text browser sees more than an empty div.
func (s *Server) servePage(w http.ResponseWriter, r *http.Request, status int, path, country string, t *store.Tracker) {
	page := s.fallbackDoc(r.Context(), status, path, country, t)
	if wantsText(r) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(status)
		w.Write(renderDocText(page))
		return
	}

	shell, err := s.shell()
	if err != nil {
		s.serverError(w, err)
		return
	}
	// A name can need escaping even though a hostname should not.
	loc := path
	if t != nil {
		loc = trackerPrefix + url.PathEscape(t.Name)
	}
	base := s.baseURL(r)
	title, desc := pageMeta(path, country, t)
	h := head{
		Title:       title,
		Description: desc,
		URL:         base + loc + canonicalQuery(country),
		Image:       base + "/og-image.png",
		NoIndex:     status != http.StatusOK,
		Body:        renderDocHTML(page),
	}
	// A 404 describes nothing, so it carries no structured data either.
	if !h.NoIndex {
		h.LD = s.jsonLD(r.Context(), base, path, country, t)
	}
	body := renderShell(shell, h)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// The shell names hashed assets, so it must never be held.
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(status)
	w.Write(body)
}

// wantsText reports whether the client wants the page as plain text. curl,
// wget and httpie send "Accept: */*" and never name text/html; lynx does name
// it and gets the HTML. ?format=txt says so outright, for testing and for a
// browser that wants the terminal form.
func wantsText(r *http.Request) bool {
	if f := r.URL.Query().Get("format"); f != "" {
		return f == "txt" || f == "text"
	}
	accept := r.Header.Get("Accept")
	return accept != "" && !strings.Contains(accept, "text/html")
}

// canonicalQuery keeps the one parameter that makes a different page.
func canonicalQuery(country string) string {
	if country == "" {
		return ""
	}
	return "?country=" + url.QueryEscape(country)
}

// lookupTracker returns the tracker a detail page is about, or nil. A retired
// name still has one.
func (s *Server) lookupTracker(ctx context.Context, name string) *store.Tracker {
	t, err := s.Store.TrackerByName(ctx, name)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			s.logger().Error("page lookup failed", "name", name, "err", err)
		}
		return nil
	}
	return &t
}

// contentTypeFor covers the extensions Go's mime table does not know. An empty
// result leaves the choice to http.ServeContent.
func contentTypeFor(path string) string {
	if strings.HasSuffix(path, ".webmanifest") {
		return "application/manifest+json"
	}
	return ""
}

// cacheFor is how long a static file may be held. Embedded files carry no
// mtime, so this header is all a browser has to go on.
func cacheFor(path string) string {
	// Vite hashes these into their names; a change is a new URL.
	if strings.HasPrefix(path, "/assets/") {
		return "public, max-age=31536000, immutable"
	}
	return "public, max-age=86400"
}

// spaHandler serves the built frontend. A path the app renders gets the shell
// and a 200; anything else gets the shell and a 404, so a deep link works and
// a bad one is still told it is bad.
func (s *Server) spaHandler() http.Handler {
	files := http.FileServer(http.FS(s.Static))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path

		// A real file wins: the assets, robots.txt, the icons.
		if p != "/" {
			if st, err := fs.Stat(s.Static, strings.TrimPrefix(p, "/")); err == nil && !st.IsDir() {
				w.Header().Set("Cache-Control", cacheFor(p))
				if ct := contentTypeFor(p); ct != "" {
					w.Header().Set("Content-Type", ct)
				}
				files.ServeHTTP(w, r)
				return
			}
		}

		c := canonicalPath(p)
		name, ok := docRoute(c)

		// Redirect only towards a page: '/t/' canonicalises to '/t', which is
		// not one, and a 301 to a 404 helps nobody.
		if ok && c != p {
			target := c
			if r.URL.RawQuery != "" {
				target += "?" + r.URL.RawQuery
			}
			http.Redirect(w, r, target, http.StatusMovedPermanently)
			return
		}

		if !ok {
			s.servePage(w, r, http.StatusNotFound, c, "", nil)
			return
		}

		var t *store.Tracker
		if name != "" {
			if t = s.lookupTracker(r.Context(), name); t == nil {
				s.servePage(w, r, http.StatusNotFound, c, "", nil)
				return
			}
		}
		// The filter only means anything on the list, and only it is kept: any
		// other parameter would mint a duplicate of the same page.
		country := ""
		if c == pathTrackers {
			country = r.URL.Query().Get("country")
		}
		s.servePage(w, r, http.StatusOK, c, country, t)
	})
}
