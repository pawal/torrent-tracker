import { test } from 'node:test'
import assert from 'node:assert/strict'

import {
  classReason, collapseChanges, describeNetwork, fmtAgo, fmtSpan, healthOf,
  isChurn, rollingFamilies, sortTrackers, trackerClass,
} from './api.js'

// The label sits next to "AS13335", so it should name the company that holds
// the AS. RDAP's org is as often the maintainer handle: AS24940 comes back as
// "HOS-GUN", which tells a reader nothing, while the Cymru AS name has the
// company in it. The two disagree on a good third of the RIPE-registered
// addresses in the collection, and the handle always looks like a typo.
test('the AS name beats the maintainer handle', () => {
  assert.equal(
    describeNetwork({ asn: 24940, org: 'HOS-GUN', as_name: 'HETZNER-AS - Hetzner Online GmbH, DE' }),
    'AS24940 Hetzner Online GmbH',
  )
})

test('the resolved holder still wins, and org is the fallback', () => {
  // The server resolves one label per AS and sends it as `holder`; the list
  // view has only that field.
  assert.equal(describeNetwork({ asn: 13335, holder: 'Cloudflare, Inc.' }), 'AS13335 Cloudflare, Inc.')
  // No Cymru name for the AS: org is better than nothing.
  assert.equal(describeNetwork({ asn: 64496, org: 'Example BV' }), 'AS64496 Example BV')
  assert.equal(describeNetwork({ asn: 64496, network_name: 'EXAMPLE-NET' }), 'AS64496 EXAMPLE-NET')
})

test('a network with no AS and no name renders to nothing', () => {
  assert.equal(describeNetwork({}), '')
  assert.equal(describeNetwork(null), '')
  assert.equal(describeNetwork({ asn: 64496 }), 'AS64496')
})

// The feed used to spend 168px a row on a full ISO stamp, 200 rows deep. The
// exact time moves to the title; what a reader scans for is how fresh a row is.
test('relative times shorten with age and fall back to the date', () => {
  const now = Date.parse('2026-08-31T12:00:00Z')
  const ago = (iso) => fmtAgo(iso, now)
  assert.equal(ago('2026-08-31T11:59:40Z'), 'just now')
  assert.equal(ago('2026-08-31T11:48:00Z'), '12m ago')
  assert.equal(ago('2026-08-31T07:00:00Z'), '5h ago')
  assert.equal(ago('2026-08-28T12:00:00Z'), '3d ago')
  // Past a month "43d ago" says less than the date does.
  assert.equal(ago('2026-06-01T12:00:00Z'), '2026-06-01')
})

// Clock skew between the browser and the server must not print "-1m ago".
test('a stamp from the future reads as just now', () => {
  const now = Date.parse('2026-08-31T12:00:00Z')
  assert.equal(fmtAgo('2026-08-31T12:00:30Z', now), 'just now')
  assert.equal(fmtAgo('nonsense', now), 'nonsense')
})

// The JS fold and the Go one render the same feed, so they agree on what a run
// is: the same name repeating the same kind of change, three deep.
test('a repeated run folds into one row at its newest entry', () => {
  const changes = [
    { id: 6, tracker_id: 1, tracker: 'toggler', type: 'bep34_changed', observed_at: '2026-08-31T12:00:00Z' },
    { id: 5, tracker_id: 2, tracker: 'quiet', type: 'tracker_added', observed_at: '2026-08-31T11:59:00Z' },
    { id: 4, tracker_id: 1, tracker: 'toggler', type: 'bep34_changed', observed_at: '2026-08-31T11:58:00Z' },
    { id: 3, tracker_id: 1, tracker: 'toggler', type: 'bep34_changed', observed_at: '2026-08-31T11:57:00Z' },
    { id: 2, tracker_id: 2, tracker: 'quiet', type: 'ip_added', observed_at: '2026-08-31T11:56:00Z' },
    { id: 1, tracker_id: 2, tracker: 'quiet', type: 'ip_removed', observed_at: '2026-08-31T11:55:00Z' },
  ]

  const rows = collapseChanges(changes)
  assert.equal(rows.length, 4)
  assert.equal(rows[0].kind, 'run')
  assert.equal(rows[0].text, 'BEP 34 record changed 3×')
  assert.equal(rows[0].count, 3)
  assert.equal(rows[0].latest, '2026-08-31T12:00:00Z')
  assert.equal(rows[0].earliest, '2026-08-31T11:57:00Z')
  // Every folded entry rides along, so the row can be opened.
  assert.equal(rows[0].members.length, 3)
  // A pair stays two rows: two of a kind are facts, three are a habit.
  assert.deepEqual(rows.slice(2).map((r) => r.kind), ['one', 'one'])
})

test('one name flapping two ways is two rows', () => {
  const changes = []
  for (let i = 0; i < 3; i++) {
    const at = `2026-08-31T1${i}:00:00Z`
    changes.push({ id: i * 2, tracker_id: 1, tracker: 'a', type: 'status_changed', observed_at: at })
    changes.push({ id: i * 2 + 1, tracker_id: 1, tracker: 'a', type: 'ip_added', observed_at: at })
  }
  const rows = collapseChanges(changes)
  assert.equal(rows.length, 2)
  assert.deepEqual(rows.map((r) => r.group).sort(), ['address', 'dns'])
})

test('a span reads in the largest unit that fits, and not at all under an hour', () => {
  assert.equal(fmtSpan('2026-08-25T12:00:00Z', '2026-08-31T12:00:00Z'), 'over 6d')
  assert.equal(fmtSpan('2026-08-31T04:00:00Z', '2026-08-31T12:00:00Z'), 'over 8h')
  assert.equal(fmtSpan('2026-08-31T11:30:00Z', '2026-08-31T12:00:00Z'), '')
})

// Half the registry stopped being trackers years ago. Reading them mixed in
// with the live ones is what made the list a graveyard, so they get their own
// section and the classification decides which one a name lands in.
test('a name on the registry is classified before it is listed', () => {
  assert.equal(trackerClass({ name: 'a', enabled: true }), 'tracker')
  assert.equal(trackerClass({ name: 'a', enabled: true, parked: true }), 'parked')
  assert.equal(trackerClass({ name: 'a', enabled: true, bep34_denies: true }), 'denies')
  assert.equal(trackerClass({ name: 'a', enabled: false }), 'retired')
  // Retired wins over parked: collection has stopped either way, and that is
  // the fact that explains why nothing about it is moving.
  assert.equal(trackerClass({ name: 'a', enabled: false, parked: true }), 'retired')
  assert.match(classReason({ enabled: true, parked: true }), /parking addresses/)
  assert.equal(classReason({ enabled: true }), '')
})

// The uptime column is bimodal — 82 names at 100%, 134 at 0%, 33 in between —
// so the band worth a chip is the middle one. An answering name with failed
// attempts behind it belongs there too, however round its uptime reads.
test('health buckets separate flapping from answering', () => {
  const answering = { state: { answering: true }, uptime: 1, misses: 0 }
  assert.equal(healthOf(answering), 'answering')
  assert.equal(healthOf({ ...answering, uptime: 0.64 }), 'flapping')
  assert.equal(healthOf({ ...answering, misses: 3 }), 'flapping')
  assert.equal(healthOf({ state: { answering: false }, last_live_at: null }), 'never')
  assert.equal(healthOf({ state: { answering: false }, last_live_at: '2026-08-01T00:00:00Z' }), 'quiet')
  assert.equal(healthOf({}), 'unprobed')
})

test('sorting is stable and never ranks an unmeasured name as a bad one', () => {
  const list = [
    { name: 'c', uptime: 0.5 },
    { name: 'a', uptime: null },
    { name: 'b', uptime: 1 },
    { name: 'd', uptime: 0.5 },
  ]
  // Nothing measured is no number, not a low one, so it sorts last whichever
  // way the column points.
  assert.deepEqual(sortTrackers(list, 'uptime', -1).map((t) => t.name), ['b', 'c', 'd', 'a'])
  assert.deepEqual(sortTrackers(list, 'uptime', 1).map((t) => t.name), ['c', 'd', 'b', 'a'])
  // Equal values keep name order rather than shuffling between renders.
  assert.deepEqual(sortTrackers(list, 'name', 1).map((t) => t.name), ['a', 'b', 'c', 'd'])
})

// p4p.arenabg.com holds 184 address intervals of which 5 are active: the rest
// are CDN edges inside a prefix the name is now tracked by. The model says
// churn inside a prefix is not reported, and the address table was reporting
// all of it.
test('churn inside a tracked prefix is told apart from address history', () => {
  const records = [
    { id: 1, ip: '2600:9000:2094::/48', family: 6, active: true, is_prefix: true },
    { id: 2, ip: '65.9.46.42', family: 4, active: true, is_prefix: false },
    { id: 3, ip: '2600:9000:2094::1', family: 6, active: false, is_prefix: false },
    { id: 4, ip: '65.9.46.99', family: 4, active: false, is_prefix: false },
    { id: 5, ip: '2600:9000:2000::/48', family: 6, active: false, is_prefix: true },
  ]
  const rolling = rollingFamilies(records)
  assert.deepEqual([...rolling], [6])

  const churn = records.filter((r) => isChurn(r, rolling))
  // Only the retired v6 address: the v4 family is not rolling, so its retired
  // address is history rather than churn, and a retired *prefix* is a move
  // between prefixes, which is exactly what the feed does report.
  assert.deepEqual(churn.map((r) => r.id), [3])
})

test('a name with no prefix record has no churn to hide', () => {
  const records = [{ id: 1, ip: '1.2.3.4', family: 4, active: false, is_prefix: false }]
  const rolling = rollingFamilies(records)
  assert.equal(rolling.size, 0)
  assert.equal(isChurn(records[0], rolling), false)
})
