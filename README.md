# torrent-tracker

Tracks the IP addresses of known BitTorrent trackers over time, checks which of
them still answer, and serves the result as a web UI, a JSON API and announce
lists a client can paste.

Per tracker name:

- **Addresses** — A and AAAA, stored as intervals so a gap stays visible
- **Placement** — origin AS, RIR, country and city, per address
- **Reachability** — BEP 15 and BEP 48 probes, per endpoint and per address
- **Preferences** — the BEP 34 record the host publishes about itself
- **Software** — the fingerprint an HTTP tracker leaves in its replies

Collection runs hourly, probing every six hours, and every change is appended to
a feed. Everything ships as one static Go binary plus a SQLite file. A public
instance runs at <https://tracker.evilbit.de/>; the original Perl version is
kept in `legacy/`.

## Quick start

```sh
make build                      # builds the UI, embeds it, builds ./trackerd
./trackerd import --file list.txt
./trackerd poll                 # one collection pass, then probe
./trackerd enrich               # look up AS, RIR and location
./trackerd serve                # UI + API on :8080, collecting hourly
```

`make run` does the last step for you. Open <http://localhost:8080>.

## The history model

Addresses are stored as intervals, not snapshots:

- **`ip_records`** — one row per contiguous period an address was seen
  (`first_seen`, `last_seen`, `active`). An address that goes away and comes
  back gets a second row, so the gap stays visible.
- **`changes`** — an append-only feed of `ip_added`, `ip_removed`,
  `status_changed` and `tracker_added`. This is what the dashboard renders.
- **`lookups`** and **`runs`** — the audit trail, so a resolver outage can be
  told apart from trackers that really disappeared.

**A failed query never retires an address.** Results are tracked per address
family: an AAAA SERVFAIL leaves the stored IPv6 records alone. NXDOMAIN and
NOERROR are authoritative and do retire.

**An address must be missing repeatedly to be retired.** `--miss-threshold`
(default 2) sets how many consecutive absences it takes, which keeps rotating
and round-robin DNS out of the feed.

## Rolling addresses

A tracker behind a CDN answers with different edge addresses every TTL, and
`p4p.arenabg.com` alone would write about 70,000 address records a year that way.
After `--roll-after` (3) changed runs the family switches to one record per
prefix:

```
p4p.arenabg.com
  IPv4  65.9.46.42, .62, .78, .93      stable
  IPv6  2600:9000:2094::/48  rolling   ~8 addresses per run
```

The prefix is not looked up, since a rolling host answers with addresses nothing
has ever seen. Containment supplies it instead: an address is matched against the
prefixes enrichment has already recorded for the name's other addresses, so one
enriched address places every later one in the /48. An address inside no known
prefix stays an address.

Churn inside a prefix is not reported. A move to another prefix is a
`prefix_added` and a `prefix_removed`. After `--steady-after` (3) settled runs
the family returns to per-address tracking; `--roll-after=-1` keeps every
address.

The changed runs need not be consecutive, since a CDN that swaps pools every
few hours never produces adjacent changes. Churn therefore survives an
unchanged run and clears only once a family has settled.

## Parked names

Expired tracker domains get bought and pointed at a parking host, where they
carry on answering and so carry on looking healthy. The detector is
self-maintaining: a control name is one known not to be a tracker, so whatever it
answers with is a parking address by definition, and any name resolving only to
those is parked.

```sh
trackerd control 0123456789nonexistent.com   # mark a canary (the seed one is automatic)
trackerd parked                              # list what it caught
trackerd parked --disable                    # remove them, keeping their history
```

The seed list has carried `0123456789nonexistent.com` since 2012 as a name meant
never to resolve. It resolves now, and 26 dead trackers answer with the same
address.

Control names are resolved every pass but stay out of the listings, the counts
and the feed. A name answering with a parking address *and* one of its own is
left alone.

Catching an operator by its addresses survives renumbering, but only reaches
operators a control name points at. Names parked elsewhere need `trackerd rm`,
or promote one of them to a control name to catch its cluster.

## Does it still answer?

Resolving in DNS and being a tracker are different questions, and dead trackers
keep their names for years. So the check is the protocol itself:

| Transport | Check | Live means |
| --- | --- | --- |
| **UDP** | BEP 15 connect handshake, 16 bytes each way | a reply carrying our transaction id |
| **HTTP/HTTPS** | BEP 48 scrape, falling back to announce | a bencoded reply, including a failure reason |

```sh
trackerd probe                  # one pass over every endpoint
trackerd reach --state partial  # the interesting ones
```

Probes run per (endpoint, address) pair, the same grain as enrichment, and roll
up per name:

```
tracker.bt4g.com                       partial
  http:2095   live on 4 of 4 addresses
  https:443   live on 2 of 4 addresses
```

That grain makes `AAAA exists but nothing listens on it` visible, and holds a
name with one stale A record at `partial` instead of flapping.

- **A probe that could not be made is not a failed probe.** A name resolving to
  nothing, or a CDN answering `429`, records `unknown` and keeps the last real
  measurement.
- **One silence is not death.** `--probe-miss-threshold` (default 2) failures
  are needed before an endpoint is called dead.
- **A silent UDP connect is retransmitted once**, inside the same
  `--probe-timeout` budget. An answer that merely is not a tracker reply is not
  retried.
- **Rolling families are sampled**, `--probe-sample` (default 2) addresses each.
- **Only the rollup reaches the feed**: `tracker_up`, `tracker_down`,
  `tracker_partial`. Per-address detail stays on the tracker page.
- **A verdict that changes is kept.** `probes` holds the open interval per
  (endpoint, address), and the stretch each new verdict replaces is appended to
  `probe_history` as a closed interval — the shape `ip_records` uses.

A database from before this existed has no endpoints. `--endpoints-only`
backfills them without re-enabling the names removed for being dead or parked:

```sh
trackerd import --file list.txt --endpoints-only
```

`poll` probes straight after collecting. Under `serve` the two run on separate
clocks — `--probe-interval`, default 6h, against hourly collection — since
probing is several requests per tracker rather than one DNS query.
`--probe=false` skips it.

The tracker page draws one lane per endpoint and address over a fixed 7, 30 or
90 day window, with the name's DNS status on the same axis above it:

```
resolution
dns       1337.abcvg.info            ████████▓▓████████████████████   93%
tracker protocol
http:80   104.21.72.244              ████████░░████████████████████   77%
http:80   2606:4700:3032::6815:48f4  ██████░░░░░░████████         ▒▒  65%  gone
udp:6969  104.21.72.244              ██████████████████░░██████████   88%
udp:6969  172.67.136.175                            ▒▒▒▒████████████  77%
          └ 07-22      07-27      08-01      08-06      08-11
```

One axis, because the question spans both: a name that stopped answering while
resolving perfectly is a dead tracker, one that stopped when its DNS went
SERVFAIL is a broken delegation. The address history shares the window, so
*which* address it answered on lines up too.

The fixed window is also what makes a rolling name readable, `p4p.arenabg.com`'s
113 address records being 113 one-hour slivers otherwise. Records that ended
before the window opened are not drawn, and the list caps at 25 until asked for
in full.

Blank is time nobody asked: `unknown` draws grey rather than joining the
red-green scale, and the percentage is the share of *measured* time the address
answered.

### How long has that been true

The registry listing carries a sentence beside the DNS verdict: `answering 12d`
or `silent 4h`. It comes from the union of every lane's live intervals, not from
`probes.since` — a name that changed address an hour ago never stopped
answering, and merging the lanes closes the handover into the one stretch it was.

Two things it will not claim: a stretch running back to the edge of the window
says `30d+` rather than `30d`, and a name nothing is measuring now has no present
state at all.

The DNS lane needs no new collection: every pass already writes status,
duration and error per tracker to `lookups`. Consecutive samples of the same
status coalesce into intervals, so 720 hourly samples become 11 for a name with
two outages, and the same query reports median and 95th-percentile resolution
latency.

`--probe-retention` and `--lookup-retention` (90 days each) sweep at the end of
each pass. `lookups` grows by one row per tracker per pass, about 200k rows a
month on a 300-name registry.

### When the host says no

BEP 34 lets a tracker's own hostname say where it runs, in a TXT record on the
name already being resolved:

```
tracker.opentrackr.org.   TXT  "BITTORRENT UDP:1337 TCP:1337"
tracker.skynetcloud.site. TXT  "BITTORRENT DENY ALL"
```

The keyword opens the record and the words after it name the endpoints, most
preferred first. A record naming none says the host runs no trackers, the
protocol's nearest thing to an opt-out. There is no DENY keyword, so `DENY ALL`
means what a bare `BITTORRENT` means: unrecognised words, ignored by design.

A denying host stops being probed, drops off every client list, and has its open
probe intervals closed.

**It flags rather than deletes**, since an ISP can block a tracker by injecting
the record and one DNS answer should not let anyone in the resolution path
erase history. The name, its addresses and everything measured before are kept,
and the reason is shown on its page.

**A UDP preference is adopted as an endpoint**, which makes a name added bare
probeable on the operator's own say-so. `TCP:` names a port without saying
whether it speaks HTTP or HTTPS, so those are recorded and not adopted. Nothing
is ever removed on the strength of a record: a measured working endpoint
outranks a list of ports.

A SERVFAIL or timeout leaves the stored record alone; NOERROR and NXDOMAIN clear
it. Changes are `bep34_added`, `bep34_changed` and `bep34_removed`. The query
rides along with the A and AAAA lookups — one more question per name per hour,
and it never touches the tracker.

34 of the 300 seed names publish a record: 28 name a UDP port, 5 name only TCP,
one denies outright. Between them they carry three words the spec does not
define — `HTTPS:443`, `DENY` and `ALL` — all read as the comments they are.

### What software is answering

No tracker discloses a version, but the HTTP reply the prober already fetched
carries a fingerprint: the failure text an implementation chose, or the shape of
the dict it returned, are literals in its source. Recording them costs no extra
request:

```
26  no info_hash parameter supplied                          (opentracker)
 7  complete,downloaded,incomplete,interval,min interval,peers
 4  files
 3  scrape requires query string
 1  files,flags,flags.min_request_interval
```

18 clusters over 60 of 299 trackers on the seed list. Thin on purpose: UDP
endpoints disclose nothing and only live HTTP ones answer, so this is a sample of
one transport rather than a census.

**Failure texts and reply shapes are not worth the same**, so which kind a
signature is gets recorded with it. A failure text is a literal from somebody's
source; a shape is only the keys the answer happened to carry, some of which
follow the peers a tracker has to report rather than the software. So shapes
group with their conditional keys dropped (`peers`, `peers6`, `downloaded`,
`min interval`, `warning message`, `external ip`, `tracker id`), which folded
ten rows into one, while failure texts group verbatim. Raw signatures are kept
as the cluster's variants, so a fold can be inspected.

**Names are applied at render time**, since a guess written into history cannot
be corrected. The signature is stored and the mapping to a name like
`opentracker` lives in `software` in `web/src/lib/api.js`: extend it there and
every stored row is reinterpreted. Anything unnamed displays as its signature.

**A shape-only tracker is asked once more**, for an announce with the
`info_hash` left out, since implementations write their own words when they
*refuse* something. Of 30 shape-only names, 17 disclosed a literal. The request
is skipped for anything already named and cannot change the verdict.

Two edges are handled. Only a prefix of a reply is ever read, so a tracker that
answers scrape with its whole table — one reply was 53MB — still yields a
fingerprint from a dictionary cut off mid-way. And every scrape reply opens with
`files`, so a shape amounting to nothing more asks announce as well.

The `Server` header is captured alongside but names the front end: nginx and
Cloudflare overwrite whatever the tracker set.

Software shows on a tracker's page, per endpoint only when they disagree, and
aggregates under "By tracker software" on the networks page. It is an inference,
so it renders in the muted style `parked` and `rolling` use.

## Address enrichment

Every observed address is annotated with its origin AS, the RIR that allocated
the prefix, and its location, so a new address that is also a new network shows
as one. Three sources, each switchable on its own:

| Source | Gives | Cost |
| --- | --- | --- |
| **Team Cymru** (`--cymru`, default on) | origin AS, AS name, BGP prefix, RIR, country, allocation date | DNS TXT lookups, keyless, ~13 ms each |
| **RDAP** (`--rdap`, default on) | authoritative network name, holder organisation, country | one HTTPS request per address, throttled to `--rdap-interval` (1 s) |
| **MaxMind** (`--geoip-db PATH`, off) | city and coordinates | local `.mmdb`, needs a free GeoLite2 account |

Cymru alone covers the common case: 448 addresses in about 34 s. RDAP is
authoritative but rate-limited, so it is the slow part; `--rdap=false` if AS and
country are enough. RDAP finds the right registry from IANA's bootstrap tables
(`data.iana.org/rdap/`), fetched once and cached, rather than through a
third-party redirector.

Under `serve`, enrichment runs after each collection pass, capped at
`--enrich-batch` (250) addresses, and refreshes anything older than
`--enrich-max-age` (30 days). An address that changes origin AS is an
`asn_changed` against every tracker pointing at it; a lookup that simply fails to
determine the AS is not a move.

`trackerd networks` summarises where the tracked hosts actually live:

```
Top networks
AS        HOLDER                                    TRACKERS  ADDRESSES
AS13335   CLOUDFLARENET - Cloudflare, Inc., US      45        170
AS396982  GOOGLE-CLOUD-PLATFORM - Google LLC, US    31        4
AS24940   HETZNER-AS - Hetzner Online GmbH, DE      13        19
```

On the networks page each country links through to its trackers. The rollups
count only the names the registry still lists, so the count and the list agree;
a tracker served from several countries appears under each.

## Names that are the same host

An address answering for two names is one machine, one operator and one outage,
however unrelated the names look.

```sh
trackerd shared                 # addresses more than one tracker answers on
trackerd shared --since 168h    # a week, rather than the default two days
```

```
ADDRESS         NAMES  STILL  NETWORK                 TRACKERS
211.75.205.187  3      yes    AS3462 HINET Network-A  tracker.dler.com, tracker.dler.org, tracker2.dler.org
188.114.96.1    6      yes    AS13335 MNT-CLOUDFLARE  supertracker.cc.cd, thebox.bz, torrentsmd.com, ...
```

**The network is what tells a host from a front end.** Three `dler` names on a
HiNet address are one server; six names on a Cloudflare edge are six origins
behind one anycast address. Only the first is one operator, though both are one
failure domain.

**A sighting counts for two days, not for this minute**, because a host handing
out a rotating subset of its addresses drops one from a name and not from its
sibling. An address only some of the names still answer on is listed as no longer
current rather than dropped.

Prefix records are left out — a shared /48 is a shared CDN — and so are parked
names, which share their parking address by definition. A parking operator no
control name points at still turns up here as the cluster it is, which is how
to find the name worth promoting.

34 addresses on the seed list are shared, covering 67 of the 300 names: 26 of
those names are one parking cluster, 4 of the addresses are Cloudflare edges, and
18 of the 34 are plain pairs — one operator, two names, one machine.

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
  shared     list addresses several trackers answer on [-n N --since D --json]
  control    list or set the control names          [--unset]
  sources    list the built-in public tracker lists
```

`add` and `import` accept full announce URLs, tracking the hostname and keeping
the scheme, port and path as an endpoint to probe, so
`udp://tracker.example.com:1337/announce` pastes straight in. A bare hostname is
tracked but cannot be probed. IP literals and `.i2p` / `.onion` / `.ygg`
addresses are skipped, having no DNS history to track.

`rm` disables a tracker but keeps its history, and re-adding the name brings it
back; `--purge` deletes it outright. The database path also comes from
`$TRACKERD_DB`.

Collection flags (`serve`, `poll`): `--resolver` (comma-separated, defaults to
`/etc/resolv.conf`), `--timeout`, `--retries`, `--workers`, `--miss-threshold`,
`--roll-after`, `--steady-after`, `--lookup-retention`. `serve` adds `--addr`,
`--interval` and `--no-collect`.

Probing flags (`probe`, `poll`, `serve`): `--probe-timeout`, `--probe-workers`,
`--probe-fanout`, `--probe-miss-threshold`, `--probe-sample`,
`--probe-retention`. `poll` and `serve` add `--probe`; `serve` adds
`--probe-interval`.

Enrichment flags (`serve`, `poll`, `enrich`): `--cymru`, `--rdap`, `--geoip-db`,
`--enrich-max-age`, `--enrich-batch`, `--enrich-workers`, `--rdap-interval`.

`--probe-workers` (8) is how many trackers are probed at once and
`--probe-fanout` (4) how many of one tracker's endpoint-and-address pairs, so at
most 32 probes are in flight and no CDN-fronted name sees its whole address set
arrive as one burst.

## Tracker lists

`list.txt` is the seed list: 326 announce URLs covering 300 unique hostnames,
merged from the original 2012 list plus three maintained public sources. The 64
names that resolved NXDOMAIN have been dropped; names that merely fail to answer
(SERVFAIL, timeout) are kept, a broken delegation not being the same as a name
that no longer exists. Import any source directly:

```sh
./trackerd import --url ngosang      # github.com/ngosang/trackerslist
./trackerd import --url xiu2         # github.com/XIU2/TrackersListCollection
./trackerd import --url newtrackon   # newtrackon.com/api/all
./trackerd import --url https://example.com/my-list.txt
```

## Lists for clients

The lists answer the question a BitTorrent client asks: which announce URLs are
worth using? Plain text, one URL per entry with a blank line after it, so a
response pastes straight into a client's tracker box.

| Endpoint | Returns |
| --- | --- |
| `GET /api/list` | the stable list, the one worth recommending |
| `GET /api/list/stable` | uptime of 95% or better, tracked for at least 10 days |
| `GET /api/list/live` | every endpoint answering right now, however new |
| `GET /api/list/{0-100}` | uptime at or above that percentage |
| `GET /api/list/udp` | the stable list, UDP only |
| `GET /api/list/http` | the stable list, HTTP and HTTPS |
| `GET /api/list/all` | every endpoint on record, dead or alive |

| Parameter | Default | Effect |
| --- | --- | --- |
| `min_age_days` | 10 on `stable`, else 0 | how long a name must have been tracked |
| `include_ipv4_only_trackers` | true | off requires an IPv6 address |
| `include_ipv6_only_trackers` | true | off requires an IPv4 address |
| `days` | 30 | the window uptime is measured over |
| `per_as` | unlimited | most trackers to take from any one origin AS |

**Uptime is the share of measured time the name answered**, over `?days`, not the
share of checks: probes are hours apart and irregular, so counting them makes two
trackers on different schedules incomparable. Unknown verdicts abstain as they do
in the rollup.

**Lanes are unioned rather than averaged.** A client needs one (endpoint,
address) pair to work, so a name with four addresses of which one is stale is
up, not 75% up. `/api/trackers` carries the same number as `uptime`, null when
nothing was measured — which is not zero, and why a name nothing has ever
spoken to is never recommended however long it has resolved.

**Entries are endpoints rather than names**, because `tracker.bt4g.com` can be
live on `http:2095` and dead on `https:443`.

**`per_as` spends the placement data.** Announcing to forty names behind one CDN
is a single failure domain wearing forty hats; `per_as=3` takes the seed list
from 323 endpoints to 216. A tracker whose network is unknown is never dropped,
and the endpoints of one hostname count once.

**The age floor is clamped to the history held.** A ten-day floor on a database
a week old drops every name, so a young database serves the best list it can
instead of nothing.

**A body can lead with `#` comment lines**, saying that the age floor was
relaxed or why the list came back empty. Clients ignore them; the URLs follow
after a blank line.

Parked, disabled and control names never appear, nor do hosts that deny
BitTorrent traffic in DNS. Nothing is stable until something has been measured,
so `stable` and the per-scheme lists come back empty on a database `probe` has
never run against; `all` works from the registry alone.

## HTTP API

Read-only. Everything that changes the registry lives in the CLI, so the server
needs no authentication.

| Endpoint | Returns |
| --- | --- |
| `GET /api/stats` | counters and the last run |
| `GET /api/trackers` | all trackers with their live addresses and uptime (`?all=1` includes removed, `?days=N` sets the uptime window) |
| `GET /api/trackers/{name}` | one tracker with full address history, change log, per-address network info, per-endpoint probe results, and probe and DNS history for the window (`?days=N`, default 30) |
| `GET /api/changes` | the change feed (`?since=RFC3339&limit=N`) |
| `GET /api/networks` | top ASes, RIR and country breakdown, enrichment coverage, reachability totals, tracker software, shared addresses |
| `GET /api/list/...` | announce URLs as plain text, see [Lists for clients](#lists-for-clients) |
| `GET /api/runs` | recent collection runs |
| `GET /api/version` | the build's version and the DNS library behind it |
| `GET /healthz` | liveness |

`limit` has a default per endpoint and is capped at 1000; anything unparseable or
non-positive falls back to the default. Every `/api/` response carries
`Access-Control-Allow-Origin: *`, so any site can read the data from the browser.

## Running as a service

`deploy/trackerd.service` is a systemd unit for Debian 13. It runs the daemon as
an unprivileged `tracker` user with its database in `/var/lib/trackerd`, which
systemd creates on first start. The UI is embedded in the binary, so a deployment
is one file plus the unit.

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
trackerd ...`. Leave it out and they will quietly create a second database in the
current directory.

The unit listens on `127.0.0.1:8080` and expects a reverse proxy in front:

```nginx
location / {
    proxy_pass http://127.0.0.1:8080;
    proxy_set_header Host $host;
}
```

To change the port, override `ExecStart` in a drop-in with `sudo systemctl edit
trackerd` rather than editing the installed unit:

```ini
[Service]
ExecStart=
ExecStart=/usr/local/bin/trackerd serve --addr :9090
```

The empty `ExecStart=` is required: a drop-in appends to list settings, and two
`ExecStart` lines are an error for anything but `Type=oneshot`. Restart and check
the result with `systemctl cat trackerd`.

The unit runs with `ProtectSystem=strict`, an empty capability set and a
`@system-service` syscall filter; the daemon only needs outbound DNS and HTTPS
plus its own state directory. A port below 1024 therefore needs
`CAP_NET_BIND_SERVICE` in both `CapabilityBoundingSet=` and
`AmbientCapabilities=`, which is reason enough to leave the proxy owning 80 and
443.

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
and a frontend build that fails if `web/dist` is out of date with `web/src`. It
also runs `govulncheck` weekly, since an advisory published tomorrow will not
wait for the next commit — a reachability check rather than a dependency-graph
scan, so it reports only what the code can reach.

The frontend build output in `web/dist/` is committed so `go build` and
`go install` work without Node installed. Rebuild it with `make ui` after
changing anything under `web/src/`.

## Layout

```
cmd/trackerd/          entry point
internal/store/        SQLite schema, migrations, queries
internal/resolver/     DNS lookups (codeberg.org/miekg/dns)
internal/bep34/        BEP 34 tracker preferences published in TXT records
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
