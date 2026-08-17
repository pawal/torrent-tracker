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
    throw new Error(msg)
  }
  return res.json()
}

export const getStats = () => get('/api/stats')
export const getTrackers = () => get('/api/trackers')
export const getTracker = (name) => get(`/api/trackers/${encodeURIComponent(name)}`)
export const getChanges = (limit = 200) => get(`/api/changes?limit=${limit}`)
export const getRuns = (limit = 10) => get(`/api/runs?limit=${limit}`)
export const getNetworks = (limit = 20) => get(`/api/networks?limit=${limit}`)
export const getVersion = () => get('/api/version')

/** Render a network as "AS13335 Cloudflare, Inc." */
export function describeNetwork(n) {
  if (!n) return ''
  const parts = []
  if (n.asn) parts.push(`AS${n.asn}`)
  // Cymru AS names read "CLOUDFLARENET - Cloudflare, Inc., US". Drop the
  // handle prefix and trailing country; both are shown elsewhere.
  const holder = (n.holder ?? n.org ?? n.as_name ?? n.network_name ?? '')
    .replace(/^[A-Z0-9_-]+\s+-\s+/, '')
    .replace(/,\s*[A-Z]{2}$/, '')
  if (holder) parts.push(holder)
  return parts.join(' ')
}

// Signatures we can put a name to. Kept out of the database because naming a
// cluster is a guess: revise this and every stored row is reinterpreted.
// Anything absent renders as its raw signature, which still groups correctly.
const software = {
  'no info_hash parameter supplied': 'opentracker',
  // jpopsuki.eu answers this and sets "Server: Ocelot 1.0" — the tracker naming
  // itself in the one header a front end had not overwritten.
  'Malformed announce': 'Ocelot',
}

/** Name the tracker software behind a signature, or show the signature itself. */
export function describeSoftware(signature) {
  if (!signature) return ''
  return software[signature] ?? signature
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
