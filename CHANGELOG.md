# Changelog

Notable changes per release. Versions follow [semantic
versioning](https://semver.org/).

## 1.2.2 — 2026-10-12

`make test` failed on Node 20, the version Debian 13 ships.

### Fixed

- `make test` runs the frontend tests on Node 20. The script handed `node
  --test` a glob, which Node expands only from 21 on.

### Upgrading

Nothing to change in the unit or the proxy.

## 1.2.1 — 2026-10-12

Ten standard-library advisories reached the daemon through Go 1.26.6. This
release builds with Go 1.27, brings the dependencies current, and enforces
foreign keys on every database connection.

### Changed

- SQLite 3.53.4, through `modernc.org/sqlite` v1.60.1.
- `maxminddb-golang` v2.8.0, for the optional `--geoip-db`.
- `codeberg.org/miekg/dns` v0.6.118, which every lookup goes through.
- Svelte 5.57.2, and Vite 8 with `vite-plugin-svelte` 7: the bundle is built
  by Rolldown and the shell's inline style is minified.
- CI on `actions/checkout`, `setup-go` and `setup-node` v7.

### Fixed

- Foreign keys are enforced on every database connection, not only the first.
  A daemon write on any other could leave rows behind for a purged tracker.

### Security

- Built with Go 1.27.2, past ten standard-library advisories trackerd could
  reach, in `net/http`, `crypto/tls` and `mime/multipart`.
- `golang.org/x/net` v0.61.0 and `golang.org/x/crypto` v0.58.0, past HTTP/2 and
  SSH advisories trackerd does not reach.
- `devalue` 5.9.4 and `source-map-js` 1.2.2 in the frontend build, past
  denial-of-service advisories. Neither ends up in the bundle.

### Upgrading

Nothing to change in the unit or the proxy. Building from source needs Go
1.27.

## 1.2.0 — 2026-08-31

A week of the feed was 820 entries from 60 names, two thirds of it eight names
oscillating; the tracker list was half graveyard in alphabetical order.

### Added

- `/lists`: the announce lists as a page, every filter a control, with a copy
  button.
- Every page rendered for clients that run no JS — semantic HTML for lynx and
  crawlers, plain text for curl, `?format=txt` to ask outright.
- Uptime, failed attempts, sortable columns and health filters on the tracker
  list, over a window of 7, 30 or 90 days.
- A **Not trackers** section below it: the parked, denying and retired names.
- `records_total` on `/api/trackers/{name}`, `parked` on `/api/networks`.
- A drawn wordmark in the header.

### Changed

- The feed folds a name's repeated churn into one row with a count and a span:
  820 rows become 108, so the dashboard covers a week where it reached a day and
  a half. A tracker's own change log folds the same way.
- Feed entries are dated by how long ago they happened, the stamp in the title.
- A network is labelled by its AS holder, not RDAP's org, which is as often a
  maintainer handle: AS24940 read `HOS-GUN`, now `Hetzner Online GmbH`. One AS is
  one row per tracker.
- Parked names leave the reachability, software and network rollups. Still
  resolved, still probed, history kept: the host they are parked on runs a
  tracker of its own, and their verdicts were counting it as one of ours.
- `records` on `/api/trackers/{name}` is scoped to `?days`.
- A rolling name's address history leaves out the churn inside its own prefix,
  with a count and a button.

### Fixed

- Software is named from the string in that project's own source. `no info_hash
  parameter supplied` is Chihaya's wording, not opentracker's; five projects are
  named now, and attribution goes from 9 trackers to 41.
- A long failure text no longer sets the width of the software column.

### Upgrading

Nothing to change in the unit or the proxy. `records` on
`/api/trackers/{name}` is the window now rather than every interval on record:
pass `?days=` to widen it, or read `records_total` for the whole count.

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
