package api

import (
	"context"
	"errors"
	"io/fs"
	"net/http"
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

// servePage writes the shell under the given status.
func (s *Server) servePage(w http.ResponseWriter, r *http.Request, status int) {
	body, err := s.shell()
	if err != nil {
		s.serverError(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// The shell names hashed assets, so it must never be held.
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(status)
	w.Write(body)
}

// trackerExists says whether a detail page has anything behind it. A retired
// name still does: keeping its history is the point.
func (s *Server) trackerExists(ctx context.Context, name string) bool {
	_, err := s.Store.TrackerByName(ctx, name)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		s.logger().Error("page lookup failed", "name", name, "err", err)
	}
	return err == nil
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

		if !ok || (name != "" && !s.trackerExists(r.Context(), name)) {
			s.servePage(w, r, http.StatusNotFound)
			return
		}
		s.servePage(w, r, http.StatusOK)
	})
}
