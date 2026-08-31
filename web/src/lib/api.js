async function get(path) {
  const res = await fetch(path, { headers: { Accept: 'application/json' } })
  if (!res.ok) {
    let msg = `${res.status} ${res.statusText}`
    try {
      const body = await res.json()
      if (body.error) msg = body.error
    } catch {
      // non-JSON error body; keep the status line
    }
    // The status rides along: a 404 is a missing name, not a broken server.
    throw Object.assign(new Error(msg), { status: res.status })
  }
  return res.json()
}

export const getStats = () => get('/api/stats')
export const getTrackers = (days = 30, all = false) =>
  get(`/api/trackers?days=${days}${all ? '&all=1' : ''}`)
export const getTracker = (name, days = 30) =>
  get(`/api/trackers/${encodeURIComponent(name)}?days=${days}`)
export const getChanges = (limit = 200, sinceDays = 0) => {
  const since = sinceDays
    ? `&since=${new Date(Date.now() - sinceDays * 86_400_000).toISOString()}`
    : ''
  return get(`/api/changes?limit=${limit}${since}`)
}
export const getRuns = (limit = 10) => get(`/api/runs?limit=${limit}`)
export const getNetworks = (limit = 20) => get(`/api/networks?limit=${limit}`)
export const getVersion = () => get('/api/version')

/**
 * Probe verdicts per (endpoint, address) on a shared time axis. probe_history
 * holds the closed intervals and probes the open one, so together they cover
 * the window. Time in no interval is blank: nobody probed then.
 */
export function probeLanes(data, from, now) {
  const span = now - from
  if (!(span > 0)) return []

  const endpoints = new Map((data?.endpoints ?? []).map((e) => [e.id, e]))
  const lanes = new Map()

  function laneFor(endpointID, ip, family) {
    const key = `${endpointID}|${ip}`
    let lane = lanes.get(key)
    if (!lane) {
      const e = endpoints.get(endpointID)
      lane = {
        key,
        ip,
        family,
        endpoint: e ? `${e.scheme}:${e.port}` : `#${endpointID}`,
        scheme: e?.scheme ?? '',
        port: e?.port ?? 0,
        segments: [],
        live: 0,
        measured: 0,
        misses: 0,
        result: '',
      }
      lanes.set(key, lane)
    }
    return lane
  }

  function push(lane, iv, until, open) {
    const a = Math.max(new Date(iv.since).getTime(), from)
    const b = Math.min(until, now)
    if (!(b > a)) return
    // Unknown abstains from uptime as it does from the rollup.
    if (iv.result === 'live' || iv.result === 'dead') {
      lane.measured += b - a
      if (iv.result === 'live') lane.live += b - a
    }
    // The failed attempts the interval survived: a live stretch is not proof
    // that every round inside it answered.
    lane.misses += iv.misses ?? 0
    lane.segments.push({
      result: iv.result,
      reason: iv.reason ?? '',
      misses: iv.misses ?? 0,
      from: a,
      to: b,
      open,
      left: ((a - from) / span) * 100,
      width: ((b - a) / span) * 100,
    })
  }

  for (const iv of data?.probe_history ?? []) {
    push(laneFor(iv.endpoint_id, iv.ip, iv.family), iv, new Date(iv.until).getTime(), false)
  }
  for (const p of data?.probes ?? []) {
    const lane = laneFor(p.endpoint_id, p.ip, p.family)
    lane.result = p.result
    lane.since = p.since
    push(lane, p, now, true)
  }

  const out = [...lanes.values()].filter((l) => l.segments.length > 0)
  for (const lane of out) {
    lane.segments.sort((a, b) => a.from - b.from)
    lane.uptime = lane.measured > 0 ? lane.live / lane.measured : null
    // No open interval: the address stopped resolving, so the lane ends early.
    lane.gone = lane.result === ''
  }
  out.sort(
    (a, b) =>
      a.scheme.localeCompare(b.scheme) ||
      a.port - b.port ||
      a.family - b.family ||
      a.ip.localeCompare(b.ip),
  )
  return out
}

/**
 * The DNS status on the same axis as the probe lanes. One lane: a name resolves
 * as a whole, and the per-address question is the one the probe lanes answer.
 */
export function resolutionLane(data, from, now) {
  const span = now - from
  if (!(span > 0)) return null

  const lane = { key: 'dns', segments: [], resolved: 0, measured: 0 }
  for (const iv of data?.resolution ?? []) {
    const a = Math.max(new Date(iv.since).getTime(), from)
    const b = Math.min(new Date(iv.until).getTime(), now)
    // The newest interval has no sample bounding it, so it can be an instant.
    if (b < a) continue
    lane.measured += b - a
    if (iv.status === 'ok') lane.resolved += b - a
    lane.segments.push({
      result: iv.status,
      reason: iv.error ?? '',
      lookups: iv.lookups,
      from: a,
      to: b,
      left: ((a - from) / span) * 100,
      width: ((b - a) / span) * 100,
    })
  }
  if (lane.segments.length === 0) return null
  lane.uptime = lane.measured > 0 ? lane.resolved / lane.measured : null
  return lane
}

/**
 * Address intervals on the same axis as the probe lanes, live ones first and
 * then the most recently retired. Anything that ended before the window opened
 * is dropped: on a rolling name that is most of the table.
 */
export function addressLanes(data, from, now) {
  const span = now - from
  if (!(span > 0)) return []

  const out = []
  for (const r of data?.records ?? []) {
    const a = Math.max(new Date(r.first_seen).getTime(), from)
    const b = Math.min(r.active ? now : new Date(r.last_seen).getTime(), now)
    if (b < a) continue
    out.push({
      ...r,
      key: r.id,
      from: a,
      to: b,
      left: ((a - from) / span) * 100,
      width: ((b - a) / span) * 100,
    })
  }
  out.sort(
    (x, y) => Number(y.active) - Number(x.active) || y.to - x.to || x.ip.localeCompare(y.ip),
  )
  return out
}

const DAY = 86_400_000

/**
 * Day-boundary ticks, labelling roughly `labels` of them. Long windows drop the
 * unlabelled days: 90 hairlines read as texture, not a grid.
 */
export function axisTicks(from, now, labels = 6) {
  const span = now - from
  if (!(span > 0)) return []
  const days = Math.max(Math.round(span / DAY), 1)
  const every = Math.max(Math.ceil(days / labels), 1)

  const ticks = []
  let i = 0
  for (let t = Math.ceil(from / DAY) * DAY; t <= now; t += DAY, i++) {
    const major = i % every === 0
    if (!major && days > 45) continue
    ticks.push({
      left: ((t - from) / span) * 100,
      major,
      label: major ? new Date(t).toISOString().slice(5, 10) : '',
    })
  }
  return ticks
}

/**
 * How long a tracker's present state has held: "6h", "3d", or "3d+" when the
 * stretch runs back to the edge of the window and is therefore a lower bound.
 */
export function fmtSince(state, now = Date.now()) {
  if (!state?.since) return ''
  const mins = Math.floor((now - new Date(state.since).getTime()) / 60_000)
  let text
  if (mins < 1) text = 'just now'
  else if (mins < 60) text = `${mins}m`
  else if (mins < 1440) text = `${Math.floor(mins / 60)}h`
  else text = `${Math.floor(mins / 1440)}d`
  return state.clipped ? `${text}+` : text
}

/**
 * How long ago something happened: "12m", "5h", "3d", and the date once a
 * month has passed. The exact stamp rides in the title, so the column can be
 * narrow without losing it.
 */
export function fmtAgo(iso, now = Date.now()) {
  const t = new Date(iso).getTime()
  if (!Number.isFinite(t)) return iso
  const mins = Math.floor((now - t) / 60_000)
  if (mins < 0) return 'just now'
  if (mins < 1) return 'just now'
  if (mins < 60) return `${mins}m ago`
  if (mins < 1440) return `${Math.floor(mins / 60)}h ago`
  if (mins < 43_200) return `${Math.floor(mins / 1440)}d ago`
  return fmtDate(iso)
}

/** A share as a whole-number percentage, or a dash when nothing was measured. */
export function fmtPercent(share) {
  if (share === null || share === undefined) return '-'
  return `${Math.round(share * 100)}%`
}

/** Render a network as "AS13335 Cloudflare, Inc." */
export function describeNetwork(n) {
  if (!n) return ''
  const parts = []
  if (n.asn) parts.push(`AS${n.asn}`)
  // The AS name before org: RDAP's org is often the maintainer handle, so
  // AS24940 reads "HOS-GUN" by org and "Hetzner Online GmbH" by AS name.
  // Cymru AS names read "CLOUDFLARENET - Cloudflare, Inc., US"; drop the
  // handle prefix and trailing country, both of which are shown elsewhere.
  const holder = (n.holder ?? n.as_name ?? n.org ?? n.network_name ?? '')
    .replace(/^[A-Z0-9_-]+\s+-\s+/, '')
    .replace(/,\s*[A-Z]{2}$/, '')
  if (holder) parts.push(holder)
  return parts.join(' ')
}

// What a row rests on. A failure text is a literal from some tracker's source
// and points at one implementation; a reply shape is only the keys the answer
// carried, which gathers lookalikes but names nothing.
const evidence = {
  failure: 'failure text',
  shape: 'reply shape',
}

export function describeEvidence(kind) {
  return evidence[kind] ?? ''
}

/**
 * Tooltip for a software row: the signature it grouped on, and the raw ones
 * folded into it, so a cluster can be inspected rather than taken on trust.
 */
export function softwareTitle(s) {
  if (!s?.variants?.length) return s?.signature ?? ''
  return `${s.signature}\n\ngrouped from:\n${s.variants.join('\n')}`
}

// What a name is, when it is on the registry without being a tracker. Half the
// list is names that stopped being trackers years ago, and mixing them with the
// live ones is what made the page a graveyard to read. Mirrors trackerClass in
// internal/api/fallback.go.
export function trackerClass(t) {
  if (t.enabled === false) return 'retired'
  if (t.bep34_denies) return 'denies'
  if (t.parked) return 'parked'
  return 'tracker'
}

const classReasons = {
  parked: 'resolves only to parking addresses',
  denies: 'publishes a BEP 34 record naming no tracker',
  retired: 'collection stopped; the history stays',
}

/** Why a name is under "Not trackers". Mirrors classReason in fallback.go. */
export function classReason(t) {
  return classReasons[trackerClass(t)] ?? ''
}

/**
 * Which health bucket a tracker is in, for the filter chips. Flapping is the
 * band worth finding: uptime is bimodal — 82 names at 100%, 134 at 0%, 33 in
 * between — and the 33 are the ones a number tells you something about. An
 * answering name with failed attempts behind it flaps too, however round its
 * uptime reads.
 */
export function healthOf(t) {
  const answering = t.state?.answering ?? false
  if (answering) {
    const flaky = (t.uptime !== null && t.uptime !== undefined && t.uptime < 0.99) || t.misses > 0
    return flaky ? 'flapping' : 'answering'
  }
  if (t.state && !t.last_live_at) return 'never'
  if (t.last_live_at) return 'quiet'
  return 'unprobed'
}

export const healthLabels = {
  answering: 'answering',
  flapping: 'flapping',
  never: 'never answered',
  quiet: 'went quiet',
  unprobed: 'not probed',
}

// How a column sorts. Every key falls back to the name, so a column of equal
// values keeps one order rather than shuffling between renders.
const sortKeys = {
  name: (t) => t.name,
  health: (t) => ['answering', 'flapping', 'quiet', 'never', 'unprobed'].indexOf(healthOf(t)),
  // Null uptime is not zero: nothing was measured. It sorts after everything
  // measured, in either direction, because it is not a worse number but no
  // number. -1 would rank it below 0% descending and above it ascending.
  uptime: (t) => (t.uptime === null || t.uptime === undefined ? null : t.uptime),
  dns: (t) => t.last_status ?? '',
  checked: (t) => t.last_checked_at ?? '',
}

/** Sort a tracker list by one column. Returns a new array. */
export function sortTrackers(list, key = 'health', dir = 1) {
  const pick = sortKeys[key] ?? sortKeys.name
  const out = [...list]
  out.sort((a, b) => {
    const x = pick(a)
    const y = pick(b)
    if (x === null && y === null) return a.name.localeCompare(b.name)
    if (x === null) return 1
    if (y === null) return -1
    let d = 0
    if (typeof x === 'number') d = x - y
    else d = String(x).localeCompare(String(y))
    return d * dir || a.name.localeCompare(b.name)
  })
  return out
}

/** Served from a country: one active address is enough, as the rollup counts it. */
export function inCountry(t, country) {
  const want = country.toLowerCase()
  return (t.networks ?? []).some((n) => (n.country || 'unknown').toLowerCase() === want)
}

/** Regional-indicator flag for a two-letter country code. */
export function flag(cc) {
  if (!cc || cc.length !== 2 || !/^[A-Za-z]{2}$/.test(cc)) return ''
  const base = 0x1f1e6 - 'A'.charCodeAt(0)
  return String.fromCodePoint(
    ...cc.toUpperCase().split('').map((c) => base + c.charCodeAt(0)),
  )
}

// What a repeated change is repeating. A week of the live feed is 820 entries
// from 60 names, and two thirds of it is eight names oscillating: one toggles
// its BEP 34 record 29 times each way, another swaps between two prefixes 71
// times. Each of those is one fact about a host, not 58 pieces of news, so the
// feed folds a run into a single row. The types that fold together are the ones
// a flap alternates between; the rest are one-off facts and never fold.
const churnGroups = {
  ip_added: 'address',
  ip_removed: 'address',
  prefix_added: 'prefix',
  prefix_removed: 'prefix',
  ips_rolling: 'rolling',
  ips_stable: 'rolling',
  tracker_up: 'reach',
  tracker_down: 'reach',
  tracker_partial: 'reach',
  status_changed: 'dns',
  bep34_added: 'bep34',
  bep34_removed: 'bep34',
  bep34_changed: 'bep34',
}

const churnText = {
  address: (n) => `${n} address changes`,
  prefix: (n) => `${n} prefix changes`,
  rolling: (n) => `rolled and settled ${n}×`,
  reach: (n) => `answering verdict flapped ${n}×`,
  dns: (n) => `DNS status flapped ${n}×`,
  bep34: (n) => `BEP 34 record changed ${n}×`,
}

/** Mirrors churnGroup in internal/api/fallback.go. */
export function churnGroup(type) {
  return churnGroups[type] ?? ''
}

/**
 * The feed with each name's repeated churn folded into one row. Input is
 * newest first; a run is placed where its newest member was, so the feed still
 * reads by when something last happened.
 *
 * Rows are `{ kind: 'one', change }` or `{ kind: 'run', ... members }`, the
 * latter carrying every folded change so a row can be opened.
 */
export function collapseChanges(changes, minRun = 3) {
  const runs = new Map()
  for (const c of changes ?? []) {
    const group = churnGroup(c.type)
    if (!group) continue
    const key = `${c.tracker_id ?? c.tracker}|${group}`
    const run = runs.get(key)
    if (run) run.push(c)
    else runs.set(key, [c])
  }

  const rows = []
  const emitted = new Set()
  for (const c of changes ?? []) {
    const group = churnGroup(c.type)
    const key = `${c.tracker_id ?? c.tracker}|${group}`
    const members = group ? runs.get(key) : null
    if (!members || members.length < minRun) {
      rows.push({ kind: 'one', key: `c${c.id}`, change: c })
      continue
    }
    if (emitted.has(key)) continue
    emitted.add(key)
    rows.push({
      kind: 'run',
      key: `r${key}`,
      tracker: c.tracker,
      group,
      count: members.length,
      // Newest first, so the run opened with its last member.
      latest: c.observed_at,
      earliest: members[members.length - 1].observed_at,
      text: churnText[group](members.length),
      members,
    })
  }
  return rows
}

/** How long a folded run has been going: "over 6d", or "" under an hour. */
export function fmtSpan(earliest, latest) {
  const mins = Math.floor((new Date(latest).getTime() - new Date(earliest).getTime()) / 60_000)
  if (!Number.isFinite(mins) || mins < 60) return ''
  if (mins < 1440) return `over ${Math.floor(mins / 60)}h`
  return `over ${Math.floor(mins / 1440)}d`
}

/** Render a change the way the original Perl report did: +/- and a reason. */
export function describe(c) {
  switch (c.type) {
    case 'ip_added':
      return { sign: '+', cls: 'add', text: c.ip }
    case 'ip_removed':
      return { sign: '-', cls: 'del', text: c.ip }
    case 'status_changed':
      return { sign: '!', cls: 'status', text: c.detail }
    case 'asn_changed':
      return { sign: '~', cls: 'net', text: c.detail }
    case 'tracker_added':
      return { sign: '*', cls: 'new', text: `added${c.detail ? ` (${c.detail})` : ''}` }
    case 'tracker_retired':
      return { sign: '*', cls: 'del', text: `retired — ${c.detail}` }
    case 'prefix_added':
      return { sign: '+', cls: 'add', text: `${c.ip} (prefix)` }
    case 'prefix_removed':
      return { sign: '-', cls: 'del', text: `${c.ip} (prefix)` }
    case 'ips_rolling':
      return { sign: '~', cls: 'net', text: `IPv${c.family} rolls: ${c.detail}` }
    case 'ips_stable':
      return { sign: '~', cls: 'net', text: `IPv${c.family} ${c.detail}` }
    case 'parked':
      return { sign: '!', cls: 'status', text: c.detail || 'parked' }
    case 'bep34_added':
      return { sign: '~', cls: 'net', text: `publishes ${c.detail}` }
    case 'bep34_removed':
      return { sign: '~', cls: 'net', text: `withdrew ${c.detail}` }
    case 'bep34_changed':
      return { sign: '~', cls: 'net', text: `preferences ${c.detail}` }
    case 'tracker_up':
      return { sign: '↑', cls: 'up', text: `answering again — ${c.detail}` }
    case 'tracker_down':
      return { sign: '↓', cls: 'down', text: `stopped answering — ${c.detail}` }
    case 'tracker_partial':
      return { sign: '~', cls: 'down', text: `partly answering — ${c.detail}` }
    default:
      return { sign: '?', cls: '', text: `${c.type} ${c.detail ?? ''}` }
  }
}

export function fmtTime(iso) {
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return iso
  return d.toISOString().replace('T', ' ').slice(0, 19) + 'Z'
}

export function fmtDate(iso) {
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return iso
  return d.toISOString().slice(0, 10)
}
