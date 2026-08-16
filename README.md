# torrent-tracker

Tracks the IP addresses of known BitTorrent trackers over time, and shows what
changed.

A collector resolves every known tracker hostname on a schedule, stores each
address as a time interval rather than a snapshot, annotates every address with
the network it sits in, and appends every change to a feed. A small Svelte UI
reads that history back. Everything ships as one static Go binary plus a SQLite
file.

This replaces the original Perl version (kept in `legacy/`), which diffed two
JSON snapshots and emailed the result.

A public instance runs at <https://tracker.evilbit.de/>.

## Quick start

```sh
make build                      # builds the UI, embeds it, builds ./trackerd
./trackerd import --file list.txt
./trackerd poll                 # one collection pass
./trackerd enrich               # look up AS, RIR and location
./trackerd serve                # UI + API on :8080, collecting hourly
```

`make run` does the last step for you. Open <http://localhost:8080>.

## How the history model works

The obvious approach is to snapshot the addresses on each run and diff
consecutive snapshots. That produces a lot of noise and no history you can
query. So instead:

- **`ip_records`** holds one row per contiguous period an address was seen
  (`first_seen`, `last_seen`, `active`). An address that goes away and later
  comes back gets a *second* row, so the gap stays visible.
- **`changes`** is an append-only feed of `ip_added` / `ip_removed` /
  `status_changed` / `tracker_added`. This is what the dashboard renders.
- **`lookups`** and **`runs`** keep the audit trail, so you can tell a resolver
  outage apart from trackers that really did disappear.

Two rules keep the history honest:

**A failed query never retires an address.** Results are tracked per address
family. If the AAAA query SERVFAILs while A succeeds, the stored IPv6 records
are left alone instead of being recorded as removed. NXDOMAIN and NOERROR *are*
authoritative, so those do retire addresses. Without this, one flaky moment from
a resolver would look like every tracker dying at once.

**Addresses must be missing repeatedly before they are retired.**
`--miss-threshold` (default 2) sets how many consecutive absences it takes. Many
trackers sit behind rotating or round-robin DNS and return a different subset
each query, so a threshold above 1 keeps that churn out of the change feed.

## Rolling addresses and parked names

Two things generate history that looks like news but is not.

**Hosts that roll their addresses.** A tracker behind a CDN answers with a
different set of edge addresses every time its TTL expires. Recorded one row at
a time, `p4p.arenabg.com` alone would write about 70,000 address records and
140,000 change entries a year, all of them saying the same thing. After three
consecutive runs with a changed set, the family switches to one record per
prefix:

```
p4p.arenabg.com
  IPv4  65.9.46.42, .62, .78, .93      stable
  IPv6  2600:9000:2094::/48  rolling   ~8 addresses per run
```

The prefix comes from enrichment, but not by looking the address up: a rolling
host answers with addresses nothing has ever seen, so they are never in
`ip_info` when the pass runs. What is known is the prefix a *sibling* address
was found in, so an address is matched by containment against the prefixes
enrichment has already recorded. One enriched address in the /48 is enough to
place every later one. An address inside no known prefix is left as an address.

Nothing is reported while the addresses churn inside the prefix; a move to a
different prefix is a `prefix_added` and a `prefix_removed`. If the addresses
settle for three runs the family goes back to being tracked address by address.
`--roll-after=-1` turns the whole thing off and keeps every address.

**Names that are no longer trackers.** Expired tracker domains get bought and
pointed at a parking host, where they carry on answering and so carry on
looking healthy. The seed list has carried `0123456789nonexistent.com` since
2012 as a canary: a name meant never to resolve. It resolves now, and 26 dead
trackers answer with the same address.

That makes the detector self-maintaining. A control name is one known not to be
a tracker, so whatever it answers with is a parking address by definition, and
any name resolving only to those is parked:

```sh
trackerd control 0123456789nonexistent.com   # mark a canary (the seed one is automatic)
trackerd parked                              # list what it caught
trackerd parked --disable                    # remove them, keeping their history
```

Control names are resolved on every pass but are not trackers: they stay out of
the listings, the counts and the change feed. A tracker that answers with a
parking address *and* an address of its own is left alone, since only names
that resolve to nothing but parking are parked.

This catches a parking operator by its addresses rather than by a curated
blocklist, so it survives the operator renumbering. It only catches operators a
control name points at, though. Names parked somewhere else need `trackerd rm`,
or promote one of them to a control name to catch the rest of its cluster.

## Address enrichment

Every observed address is annotated with its origin AS, the RIR that allocated
the prefix, and its location, so you can see when a new address also means a new
network. There are three sources, and each can be switched on or off on its own:

| Source | Gives | Cost |
| --- | --- | --- |
| **Team Cymru** (`--cymru`, default on) | origin AS, AS name, BGP prefix, RIR, country, allocation date | DNS TXT lookups, keyless, ~13 ms each |
| **RDAP** (`--rdap`, default on) | authoritative network name, holder organisation, country | one HTTPS request per address, throttled to `--rdap-interval` (1 s) |
| **MaxMind** (`--geoip-db PATH`, off) | city and coordinates | local `.mmdb`, needs a free GeoLite2 account |

Cymru alone covers the common case and is fast: 448 addresses in about 34 s.
RDAP is authoritative but rate-limited, so it is the slow part. Turn it off with
`--rdap=false` if you only want AS and country.

RDAP finds the right registry from IANA's bootstrap tables
(`data.iana.org/rdap/`), fetched once and cached, instead of going through a
third-party redirector.

Enrichment runs after each collection pass under `serve`, capped at
`--enrich-batch` addresses (250) per pass, and refreshes anything older than
`--enrich-max-age` (30 days). Prefixes change hands slowly and the registries
are rate-limited, so there is nothing to gain from checking more often.

If an address changes origin AS between refreshes, that goes into the change
feed as `asn_changed` against every tracker pointing at it. A host quietly
moving from one provider to another is worth knowing about. A lookup that simply
fails to determine the AS is not treated as a move.

`trackerd networks` summarises where the tracked hosts actually live:

```
Top networks
AS        HOLDER                                    TRACKERS  ADDRESSES
AS13335   CLOUDFLARENET - Cloudflare, Inc., US      45        170
AS396982  GOOGLE-CLOUD-PLATFORM - Google LLC, US    31        4
AS24940   HETZNER-AS - Hetzner Online GmbH, DE      13        19
```

## CLI

```
trackerd [--db PATH] [-v] <command>

  serve      run the collector and the HTTP API
  poll       run a single collection pass and exit
  enrich     look up AS, RIR and location    [--all --rdap=false --geoip-db P]
  list       list known trackers             [--all --json --names]
  add        add tracker names or announce URLs
  rm         remove a tracker                [--purge]
  import     import announce URLs            [--file PATH | --url SRC] [--dry-run]
  changes    print the recent change feed    [-n N --since 24h --json]
  networks   summarise networks, RIRs and countries [-n N --json]
  parked     list names that resolve only to parking [--disable --json]
  control    list or set the control names          [--unset]
  sources    list the built-in public tracker lists
```

`add` and `import` accept full announce URLs and pull out the hostname, so you
can paste `udp://tracker.example.com:1337/announce` straight in. IP literals and
`.i2p` / `.onion` / `.ygg` addresses are skipped, since they have no DNS history
to track.

`rm` disables a tracker but keeps its history, and re-adding the name brings it
back. `--purge` deletes it and its history outright.

The database path also comes from `$TRACKERD_DB`.

Collection flags (`serve` and `poll`): `--resolver` (comma-separated, defaults
to `/etc/resolv.conf`), `--timeout`, `--retries`, `--workers`,
`--miss-threshold`, `--roll-after`, `--steady-after`. `serve` additionally
takes `--addr`, `--interval` and `--no-collect`.

## Tracker lists

`list.txt` is the seed list: 326 announce URLs covering 300 unique hostnames,
merged from the original 2012 list plus three maintained public sources. The 64
names that resolved NXDOMAIN have been dropped, most of them casualties of the
original 2012 list. Names that merely fail to answer (SERVFAIL, timeout) are
kept, because a broken delegation is not the same as a name that no longer
exists, and that difference is worth recording. Import any source directly:

```sh
./trackerd import --url ngosang      # github.com/ngosang/trackerslist
./trackerd import --url xiu2         # github.com/XIU2/TrackersListCollection
./trackerd import --url newtrackon   # newtrackon.com/api/all
./trackerd import --url https://example.com/my-list.txt
```

## HTTP API

Read-only. Everything that changes the registry lives in the CLI, so the server
needs no authentication.

| Endpoint | Returns |
| --- | --- |
| `GET /api/stats` | counters and the last run |
| `GET /api/trackers` | all trackers with their live addresses (`?all=1` includes removed) |
| `GET /api/trackers/{name}` | one tracker with full address history, change log and per-address network info |
| `GET /api/changes` | the change feed (`?since=RFC3339&limit=N`) |
| `GET /api/networks` | top ASes, RIR and country breakdown, enrichment coverage |
| `GET /api/runs` | recent collection runs |
| `GET /healthz` | liveness |

`limit` has a default per endpoint and is capped at 1000. Anything unparseable
or non-positive falls back to the default.

Every `/api/` response carries `Access-Control-Allow-Origin: *`, so any site can
read the data straight from the browser. None of it is private and the endpoints
are GET-only, so there is nothing here for another site to abuse.

## Running as a service

`deploy/trackerd.service` is a systemd unit for Debian 13. It runs the daemon as
an unprivileged `tracker` user with its database in `/var/lib/trackerd`, which
systemd creates on first start. The UI is embedded in the binary, so a
deployment is one file plus the unit, with nothing else to copy.

```sh
make build
sudo install -m 0755 trackerd /usr/local/bin/trackerd

sudo adduser --system --group --home /var/lib/trackerd --no-create-home tracker
sudo install -m 0644 deploy/trackerd.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now trackerd

systemctl status trackerd
journalctl -u trackerd -f
```

Seed the tracker list once the service is up. SQLite runs in WAL mode with a
10 s busy timeout, so the CLI and the running daemon can share the database:

```sh
sudo -u tracker trackerd --db /var/lib/trackerd/trackers.db import --file list.txt
sudo -u tracker trackerd --db /var/lib/trackerd/trackers.db import --url ngosang
```

`TRACKERD_DB` is set in the unit but not in your shell, so ad-hoc commands need
`--db`, or `sudo -u tracker env TRACKERD_DB=/var/lib/trackerd/trackers.db
trackerd ...`. Leave it out and they will quietly create a second database in
the current directory.

The unit listens on `127.0.0.1:8080` and expects a reverse proxy in front:

```nginx
location / {
    proxy_pass http://127.0.0.1:8080;
    proxy_set_header Host $host;
}
```

To change the port or listen on every interface, override `ExecStart` in a
drop-in with `sudo systemctl edit trackerd` rather than editing the installed
unit:

```ini
[Service]
ExecStart=
ExecStart=/usr/local/bin/trackerd serve --addr :9090
```

The empty `ExecStart=` is required: a drop-in appends to list settings, and two
`ExecStart` lines are an error for anything but `Type=oneshot`. Restart the
service afterwards and check the result with `systemctl cat trackerd`.

The unit runs with `ProtectSystem=strict`, an empty capability set and a
`@system-service` syscall filter; the daemon only needs outbound DNS and HTTPS
(for RDAP) plus its own state directory. A port below 1024 therefore needs
`CAP_NET_BIND_SERVICE` in both `CapabilityBoundingSet=` and
`AmbientCapabilities=`, which is a good reason to leave the daemon on a high
port and let the proxy own 80 and 443.

Nothing served over HTTP changes state. The API is read-only and the write paths
live in the CLI, so a public deployment needs no authentication.

## Development

```sh
make check      # gofmt, go vet, go test
make dev        # Vite dev server with hot reload, proxying /api to :8080
make help       # all targets
```

Run `make run` in one shell and `make dev` in another for frontend work.

GitHub Actions runs the same checks on every push and pull request
(`.github/workflows/ci.yml`): gofmt, `go vet`, `go test -race`, a static build,
and a frontend build that fails if `web/dist` is out of date with `web/src`.

The frontend build output in `web/dist/` is committed so `go build` and
`go install` work without Node installed. Rebuild it with `make ui` after
changing anything under `web/src/`.

## Layout

```
cmd/trackerd/          entry point
internal/store/        SQLite schema, migrations, queries
internal/resolver/     DNS lookups (codeberg.org/miekg/dns)
internal/enrich/       AS/RIR/geo providers: Cymru, RDAP, MaxMind
internal/collector/    scheduler, the pure diff engine, enrichment runner
internal/api/          HTTP handlers
internal/trackerlist/  announce-URL parsing and list fetching
internal/cli/          subcommands
web/                   Svelte 5 + Vite frontend, embedded via go:embed
deploy/                systemd unit
legacy/                the original Perl implementation
```

Dependencies: `codeberg.org/miekg/dns`, `modernc.org/sqlite` and
`oschwald/maxminddb-golang`, all pure Go, so `CGO_ENABLED=0` gives a static
binary that cross-compiles anywhere.

## License

BSD 2-Clause, see [LICENSE](LICENSE). Copyright 2012-2026 Patrik Wallström.
