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
changed runs, the family switches to one record per prefix:

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

**Those three changed runs need not be consecutive.** Not every CDN reshuffles
on every query. `p4p.arenabg.com` serves one CloudFront pool for a couple of
hours, swaps to another, and swaps back — so its changes are never adjacent, and
a churn count cleared by the first unchanged run went 1, 0, 1, 0 and never
reached the threshold. Its IPv4 family stayed on per-address tracking for days,
emitting four `ip_removed` and four `ip_added` entries per swap, while its IPv6
family churned every single run and rolled immediately. Three names produced 54%
of all address churn in the feed on that rule.

So churn now survives an unchanged run and clears only once a family has
actually settled — the same `--steady-after` that un-rolls a rolling family. A
set that cannot hold still for three runs is churning; one that renumbers once
and then holds is a host that moved, and still never rolls.

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

**A verdict that changes is kept, not overwritten.** The `probes` table holds
one row per (endpoint, address) and describes the present, so a tracker that
went dead last Tuesday would leave no trace of the week it was working. Each
time a verdict moves, the stretch it replaces is appended to `probe_history` as
a closed interval — the same shape `ip_records` uses for addresses, and for the
same reason: a tracker that answers for a month is one row, not one row per
pass. The open interval stays in `probes`, so the two together cover the axis
with no seam.

The tracker page draws that as one lane per endpoint and address over a fixed
7, 30 or 90 day window, with the DNS status of the name on the same axis above
it:

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

One axis rather than two cards, because the useful question spans both: a name
that stopped answering *while resolving perfectly* is a dead tracker, and one
that stopped answering *when its DNS went SERVFAIL* is a broken delegation.
Reading down a column says which. The address history below runs on the same
window and the same axis, so the third row of the question — *which* address it
was answering on — lines up with the other two.

The window is what makes the address history readable at all. Drawn over the
span of the data, a rolling name is unusable: `p4p.arenabg.com` has 113 address
records, so its timeline was 113 one-hour slivers with no scale. On a fixed
window, records that ended before it opened are simply not drawn, live ones sort
first and retired ones by how recently they went, and the list caps at the 25
most recent until asked for in full.

Blank is time nobody asked, which is not the same as asking and learning
nothing: probing starts when a name is added and stops when its address goes
away, and an `unknown` verdict draws grey rather than joining the red-green
scale. The percentage is the share of *measured* time the address answered, so
an unprobed week neither helps nor hurts it — the same abstention rule the
rollup uses.

### How long has that been true

The lanes answer it for anyone willing to read a chart, but the first thing
anyone wants from a list is a sentence, which is what newTrackon's status column
gives: *working for 3 days*, *down for 6 hours*. The registry listing carries the
same, as `answering 12d` or `silent 4h` beside the DNS verdict.

It cannot come from the current probe rows, which is the obvious place to look
and the wrong one. `probes.since` dates one address's verdict, so a name that
answered on `1.2.3.4` for a week and moved to `1.2.3.5` an hour ago would read
as answering for an hour when it never stopped. The stretch is a property of the
name, so it comes out of the same union the uptime does: merge every lane's live
intervals and take the last one, and a handover between addresses closes up into
the one stretch it was.

Two things it will not claim. A stretch running back to the edge of the window
is a lower bound and says so — `30d+`, not `30d` — because the window cannot see
when it really began. And a name nothing is measuring now has no present state
at all: when probing stops, the last verdict describes the past, and *answering
for six hours* would be a claim about six hours nobody watched.

The DNS lane needs no new collection. Every pass already wrote a row to
`lookups` — status, duration and error, per tracker — and nothing ever read it,
so a month of resolution history was on disk from the start. Consecutive
samples of the same status coalesce into one interval, which is what makes it
drawable: 720 hourly samples become 11 intervals for a name with two outages.
Each sample speaks for the time until the next one, which is all a poll can.
The same query reports median and 95th-percentile resolution latency for the
window; percentiles rather than a mean, since one resolver timeout would drag
an average past every real reading.

Both logs are bounded by retention rather than by uptime: `--probe-retention`
and `--lookup-retention` (both 90 days) sweep at the end of each probing and
collection pass. `lookups` in particular was growing without bound — one row
per tracker per pass is about 200k rows a month on a 300-name registry.

### When the host says no

BEP 34 lets a tracker's own hostname say where it runs, in a TXT record on the
name we are already resolving:

```
tracker.opentrackr.org.   TXT  "BITTORRENT UDP:1337 TCP:1337"
tracker.skynetcloud.site. TXT  "BITTORRENT DENY ALL"
```

The keyword opens the record and the words after it name the endpoints, most
preferred first. A record naming none says the host runs no trackers, which is
the closest thing the protocol has to an opt-out — there is no DENY keyword, so
`DENY ALL` means what a bare `BITTORRENT` means: those are two unrecognised
words, and unrecognised words are ignored by design.

This is worth honouring for its own sake, and it is the one thing a tracker
operator can do to be left alone by a monitor that never announces. A denying
host stops being probed, drops off every client list, and has its open probe
intervals closed rather than left to imply we are still measuring it.

**It flags rather than deletes, which is where this parts company with
newTrackon**, whose denial removes the tracker outright. The spec's own security
note is the reason: an ISP can block a tracker by injecting the record. Deleting
on the strength of one DNS answer would let anyone in the resolution path erase
history, so the name, its addresses and everything measured before it are kept
and the reason is shown on its page.

**A UDP preference is adopted as an endpoint.** It names a transport and a port
exactly, so a name that was added bare — with no announce URL and therefore
nothing to probe — becomes probeable on the operator's own say-so. A `TCP:`
preference names a port without saying whether it speaks HTTP or HTTPS, and
guessing wrong records a dead endpoint for a live tracker, so those are kept in
the record and not adopted. Nothing is ever removed on the strength of a record:
an endpoint we have measured working outranks a list of ports.

34 of the 300 names on the seed list publish one, 28 of them naming a UDP port,
5 naming only TCP, and one denying outright. Between them the 34 records carry
exactly three words the spec does not define — `HTTPS:443`, `DENY` and `ALL` —
and all three are read as the comments they are.

The same rules as everywhere else apply to the lookup. A SERVFAIL or a timeout
leaves the stored record alone, since a query that could not be answered is not
an answer of "no record"; NOERROR and NXDOMAIN are authoritative and do clear
it. Publishing, moving or withdrawing a record is a `bep34_added`,
`bep34_changed` or `bep34_removed` in the feed. The query rides along with the
A and AAAA lookups each pass, so it costs one more question per name per hour
and does not touch the tracker at all.

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
`--miss-threshold`, `--roll-after`, `--steady-after`, `--lookup-retention`.
`serve` additionally takes `--addr`, `--interval` and `--no-collect`.

Probing flags (`probe`, `poll` and `serve`): `--probe-timeout`,
`--probe-workers`, `--probe-fanout`, `--probe-miss-threshold`,
`--probe-sample`, `--probe-retention`. `poll` and `serve` additionally take
`--probe`, and `serve` takes `--probe-interval`.

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

## Lists for clients

Everything above answers questions about trackers. The lists answer the one a
BitTorrent client asks: which announce URLs are worth using? They are plain
text, one URL per entry with a blank line after it, so a response pastes
straight into a client's tracker box.

| Endpoint | Returns |
| --- | --- |
| `GET /api/list` | the stable list, the one worth recommending |
| `GET /api/list/stable` | uptime of 95% or better, tracked for at least 10 days |
| `GET /api/list/live` | every endpoint answering right now, however new |
| `GET /api/list/{0-100}` | uptime at or above that percentage |
| `GET /api/list/udp` | the stable list, UDP only |
| `GET /api/list/http` | the stable list, HTTP and HTTPS |
| `GET /api/list/all` | every endpoint on record, dead or alive |

**Uptime is the share of measured time the name answered**, over `?days` (30 by
default). Not the share of checks: probes are hours apart and irregular, so
counting them makes two trackers on different schedules incomparable, which is
the limitation newTrackon's own FAQ concedes about its last-1000-checks figure.
Unknown verdicts abstain exactly as they do in the rollup, so a week nobody
could probe neither helps nor hurts.

The lanes of a name are unioned rather than averaged. A client needs one
(endpoint, address) pair to work, not all of them, so a name with four addresses
of which one is stale is up, not 75% up. `/api/trackers` carries the same number
as `uptime`, null when nothing was measured — which is not the same as zero, and
the reason a name nothing has ever spoken to is never recommended however long
it has resolved.

Entries are endpoints rather than names, because `tracker.bt4g.com` can be live
on `http:2095` and dead on `https:443`, and a list that averaged the two would
recommend a URL that does not work. `live` asks the same question of the
endpoint rather than the name, for the same reason.

Parked, disabled and control names never appear, nor do hosts that deny
BitTorrent traffic in DNS. A parked domain resolves perfectly and answers
nothing, which is exactly what a list built on DNS alone cannot see, and a
denying host has asked not to be contacted at all.

The query parameters, the first three named as newTrackon names them so a caller
can swap the host and keep the URL:

| Parameter | Default | Effect |
| --- | --- | --- |
| `min_age_days` | 10 on `stable`, else 0 | how long a name must have been tracked |
| `include_ipv4_only_trackers` | true | off requires an IPv6 address |
| `include_ipv6_only_trackers` | true | off requires an IPv4 address |
| `days` | 30 | the window uptime is measured over |
| `per_as` | unlimited | most trackers to take from any one origin AS |

`per_as` is the one no other list can offer, since no other list knows what
network its trackers sit on. Announcing to forty names behind one CDN is a
single failure domain wearing forty hats: `per_as=3` takes the seed list from
323 endpoints to 216. A tracker whose network is unknown is never dropped,
nothing having said it adds concentration, and the endpoints of one hostname
count once.

Nothing is stable until something has been measured, so `stable` and the
per-scheme lists come back empty on a database `probe` has never run against.
`all` works from the registry alone.

## HTTP API

Read-only. Everything that changes the registry lives in the CLI, so the server
needs no authentication.

| Endpoint | Returns |
| --- | --- |
| `GET /api/stats` | counters and the last run |
| `GET /api/trackers` | all trackers with their live addresses and uptime (`?all=1` includes removed, `?days=N` sets the uptime window) |
| `GET /api/trackers/{name}` | one tracker with full address history, change log, per-address network info, per-endpoint probe results, and probe and DNS history for the window (`?days=N`, default 30) |
| `GET /api/changes` | the change feed (`?since=RFC3339&limit=N`) |
| `GET /api/networks` | top ASes, RIR and country breakdown, enrichment coverage, reachability totals, tracker software |
| `GET /api/list/...` | announce URLs as plain text, see [Lists for clients](#lists-for-clients) |
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
