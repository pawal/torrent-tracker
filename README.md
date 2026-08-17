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

## Does it still answer?

Resolving in DNS and being a tracker are different questions, and the second is
the one people mean. Dead trackers keep their names for years, so the registry
is full of hosts that answer every A query and no announce. There is no public
API that will tell you whether an arbitrary hostname is a live tracker —
newTrackon and the published lists only cover names someone submitted — so the
check is the protocol itself:

| Transport | Check | Live means |
| --- | --- | --- |
| **UDP** | BEP 15 connect handshake, 16 bytes each way | a reply carrying our transaction id |
| **HTTP/HTTPS** | BEP 48 scrape, falling back to announce | a bencoded reply, including a failure reason |

```sh
trackerd probe                  # one pass over every endpoint
trackerd reach --state partial  # the interesting ones
```

A database from before this existed has no endpoints, and `probe` will say so.
Backfill them with `--endpoints-only`, which attaches endpoints to names the
registry already has and nothing else:

```sh
trackerd import --file list.txt --endpoints-only
```

Use that rather than a plain import on a curated registry. Importing re-enables
every name in the list, which is what you want when adding trackers and exactly
what you do not want here: it would resurrect the names you removed for being
dead or parked.

**Per address, not per name.** A probe is made for each (endpoint, address)
pair, the same grain as enrichment. A name with four A records where one is a
stale host reads as `partial` rather than flapping between live and dead, and
`AAAA exists but nothing listens on it` becomes visible instead of being hidden
by happy eyeballs. Probing by name would pick whichever address the resolver
returned and call the question closed.

**One hostname, several endpoints.** `1337.abcvg.info` serves `:80` and `:443`,
and they can disagree. The importer now keeps the scheme, port and path it used
to discard, so endpoints are probed separately and rolled up:

```
tracker.bt4g.com                       partial
  http:2095   live on 4 of 4 addresses
  https:443   live on 2 of 4 addresses
```

The same two rules that keep the address history honest apply here:

**A probe that could not be made is not a failed probe.** A name that resolves
to nothing, or a CDN answering `429`, records `unknown` and keeps whatever the
last real measurement said. Only measured states reach the feed, so a resolver
outage cannot read as every tracker dying at once.

**One silence is not death.** `--probe-miss-threshold` (default 2) sets the
consecutive failures needed before an endpoint is called dead. Trackers drop
UDP packets and rate-limit; a single timeout proves nothing.

UDP gets a second chance sooner than that. A dropped datagram is
indistinguishable from a dead tracker, so a silent connect request is
retransmitted once inside the same `--probe-timeout` budget — BEP 15 expects
clients to retry, and one unlucky packet should not spend one of an endpoint's
two lives. An answer that merely fails to be a tracker reply is not retried:
asking twice would not change it.

Rolling families are the deliberate exception to probing every address. Their
records hold a prefix, and the addresses inside it are interchangeable and gone
by the next round, so `--probe-sample` (default 2) addresses are probed per
family instead of all of them.

Only the rollup reaches the change feed — `tracker_up`, `tracker_down` and
`tracker_partial` — because per-address transitions on a CDN-fronted name would
flood it. The per-endpoint, per-address detail lives on the tracker page.

### What software is answering

No tracker discloses a version, and BEP 15 has nowhere to put one. But the HTTP
reply the prober already fetched carries a fingerprint: the failure text an
implementation chose, or the shape of the dict it returned, are literals in its
source. Recording them costs no extra request:

```
26  no info_hash parameter supplied                          (opentracker)
 7  complete,downloaded,incomplete,interval,min interval,peers
 4  files
 3  scrape requires query string
 1  files,flags,flags.min_request_interval
```

18 clusters over 60 of 299 trackers on the seed list. The coverage is thin on
purpose: UDP endpoints disclose nothing, and only live HTTP ones answer at all,
so this is a sample of one transport rather than a census.

The two kinds of evidence are not worth the same, so which one a signature is
gets recorded alongside it. A **failure text** is a literal lifted from somebody's
source and points at one implementation. A **reply shape** is only the keys the
answer happened to carry, and some of those follow the peers a tracker has to
report rather than the software: `peers6` appears only when there was an IPv6
peer to list, and one tracker in the live registry answered with `peers6` and no
`peers` at all. Grouping the two alike split a single implementation across ten
rows and let a tracker drift between them from one pass to the next.

So shapes are grouped with their conditional keys — `peers`, `peers6`,
`downloaded`, `min interval`, `warning message`, `external ip`, `tracker id` —
dropped, which folded those ten rows into one; failure texts are grouped
verbatim. The raw signatures are kept and listed as the cluster's variants, so a
fold can be inspected instead of trusted. Rows recorded before the kind was
tracked have none, and group by their raw signature until the next pass.

The raw signature is what gets stored. Naming a cluster is a guess, and a guess
written into history cannot be corrected, so the mapping from signature to a
name like `opentracker` lives in `software` in `web/src/lib/api.js` and is
applied at render time — extend it there and every stored row is reinterpreted.
Anything unnamed displays as its signature, which still groups correctly.

A tracker that can answer the question asked has no reason to say who it is: the
reply is the same handful of BEP 3 keys whoever wrote it. Implementations write
their own words when they *refuse* something, so a live tracker that has offered
nothing but a shape is asked once more for an announce with the `info_hash` left
out. Of 30 names that were shape-only in the live registry, 17 disclosed a
literal when asked, 16 endpoints of them sharing one — a cluster that had been
scattered across several shape rows. The extra request is skipped for anything
already named, and whatever it draws cannot change the verdict: only the
fingerprint is taken from it.

Two things stop a live HTTP tracker from being identified, and both are handled.
Some answer scrape with their whole table rather than the `info_hash` asked
about — one observed reply was 53MB — so only a prefix is ever read and the
fingerprint has to survive being cut off mid-dictionary; the keys that arrived
are kept and the rest is discarded. And every scrape reply opens with the same
`files` key, so a scrape whose shape amounts to nothing more than that is not an
identification: announce is asked as well, without letting the answer overturn
the verdict scrape already established.

The `Server` header is captured alongside, but it names the front end rather
than the tracker: nginx and Cloudflare overwrite whatever the tracker set.

A tracker's software shows once on its page, breaking out per endpoint only
when they disagree, and aggregates under "By tracker software" on the networks
page. It is an inference, not a measurement, so it renders in the muted style
`parked` and `rolling` use rather than joining the status colours.

`poll` probes straight after collecting, so a one-shot run works from the
addresses it just found. Under `serve` the two run on separate clocks
(`--probe-interval`, default 6h, against an hourly collection): probing is
several requests per tracker and far more visible to the operator than a DNS
query, so it does not belong on the same schedule. `--probe=false` skips it.

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

On the networks page each country links through to its trackers. The rollups
count only the names the registry still lists, so the count and the list agree; a
tracker served from several countries appears under each.

## CLI

```
trackerd [--db PATH] [-v] <command>

  serve      run the collector and the HTTP API
  poll       run a single collection pass, then probe   [--probe=false]
  enrich     look up AS, RIR and location    [--all --rdap=false --geoip-db P]
  probe      check which trackers still answer      [--json]
  reach      list trackers by whether they answer   [--state S --json]
  list       list known trackers             [--all --json --names]
  add        add tracker names or announce URLs
  rm         remove a tracker                [--purge]
  import     import announce URLs            [--file PATH | --url SRC]
                                             [--dry-run --endpoints-only]
  changes    print the recent change feed    [-n N --since 24h --json]
  networks   summarise networks, RIRs and countries [-n N --json]
  parked     list names that resolve only to parking [--disable --json]
  control    list or set the control names          [--unset]
  sources    list the built-in public tracker lists
```

`add` and `import` accept full announce URLs, tracking the hostname and keeping
the scheme, port and path as an endpoint to probe, so you can paste
`udp://tracker.example.com:1337/announce` straight in. A bare hostname is
tracked but cannot be probed, since it says nothing about how to reach the
tracker. IP literals and `.i2p` / `.onion` / `.ygg` addresses are skipped, since
they have no DNS history to track.

`rm` disables a tracker but keeps its history, and re-adding the name brings it
back. `--purge` deletes it and its history outright.

The database path also comes from `$TRACKERD_DB`.

Collection flags (`serve` and `poll`): `--resolver` (comma-separated, defaults
to `/etc/resolv.conf`), `--timeout`, `--retries`, `--workers`,
`--miss-threshold`, `--roll-after`, `--steady-after`. `serve` additionally
takes `--addr`, `--interval` and `--no-collect`.

Probing flags (`probe`, `poll` and `serve`): `--probe-timeout`,
`--probe-workers`, `--probe-fanout`, `--probe-miss-threshold`,
`--probe-sample`. `poll` and `serve` additionally take `--probe`, and `serve`
takes `--probe-interval`.

`--probe-workers` (default 8) is how many trackers are probed at once;
`--probe-fanout` (default 4) is how many of one tracker's endpoint-and-address
pairs are probed at once, so at most 32 probes are ever in flight. Fan-out is
bounded per tracker on purpose: a CDN-fronted name with a dozen addresses would
otherwise see the whole set arrive as one burst and throttle us.

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
| `GET /api/trackers/{name}` | one tracker with full address history, change log, per-address network info and per-endpoint probe results |
| `GET /api/changes` | the change feed (`?since=RFC3339&limit=N`) |
| `GET /api/networks` | top ASes, RIR and country breakdown, enrichment coverage, reachability totals, tracker software |
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
make vuln       # govulncheck
make dev        # Vite dev server with hot reload, proxying /api to :8080
make help       # all targets
```

Run `make run` in one shell and `make dev` in another for frontend work.

GitHub Actions runs the same checks on every push and pull request
(`.github/workflows/ci.yml`): gofmt, `go vet`, `go test -race`, a static build,
and a frontend build that fails if `web/dist` is out of date with `web/src`.

It also runs `govulncheck`, weekly as well as on every push, since an advisory
published tomorrow will not wait for the next commit. That is a reachability
check, not a dependency-graph scan: it reports only advisories the code can
actually reach. Graph scanners will flag rather more, including modules that
are pruned before they ever reach the binary — `go version -m ./trackerd` lists
what is genuinely linked.

The frontend build output in `web/dist/` is committed so `go build` and
`go install` work without Node installed. Rebuild it with `make ui` after
changing anything under `web/src/`.

## Layout

```
cmd/trackerd/          entry point
internal/store/        SQLite schema, migrations, queries
internal/resolver/     DNS lookups (codeberg.org/miekg/dns)
internal/enrich/       AS/RIR/geo providers: Cymru, RDAP, MaxMind
internal/prober/       BEP 15 and BEP 48 checks, software fingerprinting
internal/collector/    scheduler, the pure diff engine, enrichment and probe runners
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
