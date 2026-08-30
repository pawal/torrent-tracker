package api

import (
	"strings"
	"testing"
	"testing/fstest"
)

// Embedded files have no mtime, so http.FileServer sends neither Last-Modified
// nor an ETag. Without an explicit header every visit re-downloads the bundle.
func TestStaticFilesAreCacheable(t *testing.T) {
	h, _ := testServer(t)

	// Vite hashes the asset names, so they can be held indefinitely.
	rec := get(t, h, "/assets/app.js")
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Errorf("asset Cache-Control = %q, want immutable", cc)
	}

	// The icons keep their names across builds, so they have to expire.
	rec = get(t, h, "/favicon.ico")
	cc := rec.Header().Get("Cache-Control")
	if cc == "" || strings.Contains(cc, "immutable") {
		t.Errorf("favicon Cache-Control = %q, want a bounded max-age", cc)
	}

	// The shell names the hashed assets, so holding it pins a stale build.
	rec = get(t, h, "/")
	if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("shell Cache-Control = %q, want no-cache", cc)
	}
}

func TestCacheFor(t *testing.T) {
	if got := cacheFor("/assets/index-abc123.js"); !strings.Contains(got, "31536000") {
		t.Errorf("cacheFor(asset) = %q", got)
	}
	if got := cacheFor("/og-image.png"); !strings.Contains(got, "86400") {
		t.Errorf("cacheFor(icon) = %q", got)
	}
}

// Go's mime table has no entry for .webmanifest, so http.FileServer sniffs it
// as text/plain and some browsers refuse to install the icons.
func TestWebManifestContentType(t *testing.T) {
	st := testStore(t)
	srv := &Server{Store: st, Log: discard(), Static: fstest.MapFS{
		"index.html":       &fstest.MapFile{Data: []byte("<!doctype html>")},
		"site.webmanifest": &fstest.MapFile{Data: []byte(`{"name":"x"}`)},
	}}

	rec := get(t, srv.Handler(), "/site.webmanifest")
	if ct := rec.Header().Get("Content-Type"); ct != "application/manifest+json" {
		t.Errorf("Content-Type = %q, want application/manifest+json", ct)
	}
}
