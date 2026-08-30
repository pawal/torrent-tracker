# Changelog

Notable changes per release. Versions follow [semantic
versioning](https://semver.org/).

## Unreleased

### Added

- Every page rendered server-side for clients that run no JS: semantic HTML in
  the shell for lynx and crawlers, plain text for curl.
- `?format=txt` on any page; otherwise an `Accept` naming `text/html` decides.
- Both forms carry the present state and the feed, not the windowed timelines.

## 1.1.0 — 2026-08-30

The UI moved off `#/` fragments, so every page is a real URL that crawlers and
unfurlers can see.

### Added

- Real page paths: `/`, `/trackers`, `/networks`, `/t/{name}`.
- Per-page title, description, canonical, Open Graph and a schema.org `Dataset`,
  rendered into the shell before any JS runs.
- `/robots.txt` and `/sitemap.xml`, with one entry per live tracker.
- `--base-url` on `serve`, for absolute canonical and sitemap links.
- Site icons, a web manifest and a social card.

### Changed

- An unknown path or tracker name gets a 404 and `noindex`, not the shell and a
  200; `/trackers/` redirects to `/trackers`.
- Hashed assets are cached for a year, other static files for a day; the
  manifest is served as `application/manifest+json`.

### Upgrading

Absolute links otherwise depend on request headers being right:

- Add `--base-url https://your.host` to `ExecStart`.
- Add `proxy_set_header X-Forwarded-Proto $scheme;` to the reverse proxy.

The proxy must pass every path to the daemon, not just `/` and `/api/` — tracker
pages are now server-routed.
