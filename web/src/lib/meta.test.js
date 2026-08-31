import { test } from 'node:test'
import assert from 'node:assert/strict'

import { pageMeta, trackerState } from './meta.js'
import { parseRoute } from './router.js'

const metaFor = (path, search) => pageMeta(parseRoute(path, search))

// Two pages sharing a title are two pages a search engine cannot tell apart,
// which is the whole reason the static one had to go.
test('every route has its own title', () => {
  const paths = ['/', '/trackers', '/networks', '/lists', '/t/a.example.com', '/nope']
  const titles = paths.map((p) => metaFor(p).title)
  assert.equal(new Set(titles).size, paths.length, titles.join(' | '))
})

test('the tracker name leads its own title', () => {
  const m = metaFor('/t/open.demonii.com')
  assert.match(m.title, /^open\.demonii\.com — /)
  assert.match(m.description, /open\.demonii\.com/)
})

test('the country filter is its own page', () => {
  assert.match(metaFor('/trackers', 'country=SE').title, /Trackers in SE/)
  assert.match(metaFor('/trackers', 'country=SE').description, /address in SE/)
  // 'unknown' is not a country, so it gets a sentence rather than a code.
  assert.match(metaFor('/trackers', 'country=unknown').title, /no country on record/)
})

test('an unknown path gets the not-found metadata', () => {
  assert.match(metaFor('/nope').title, /^Not found/)
  assert.match(metaFor('/t/%').title, /^Not found/)
})

// A description Google truncates is a description half-written. Titles run
// past the pixel cap on the longest tracker names, which is unavoidable and
// harmless, but nothing should be empty or unbounded.
test('descriptions stay within a sensible length', () => {
  for (const path of ['/', '/trackers', '/networks', '/t/a.example.com', '/nope']) {
    const { title, description } = metaFor(path)
    assert.ok(title.length > 0 && title.length < 120, `title: ${title}`)
    assert.ok(description.length > 20, `description: ${description}`)
    assert.ok(description.length < 200, `description too long: ${description.length}`)
  }
})

test('pageMeta survives a missing route', () => {
  assert.match(pageMeta(undefined).title, /^Not found/)
  assert.match(pageMeta({}).title, /^Not found/)
})

// The Go half of this lives in internal/api/meta.go and is tested against the
// same rows there. If the two drift, a page's description changes the moment
// the client finishes loading, and a crawler and a reader see different text.
test('trackerState follows the tracker, matching meta.go', () => {
  const cases = [
    [{ reach: 'live' }, 'resolves and answers the tracker protocol'],
    [{ reach: 'partial' }, 'answers on some of its addresses'],
    [{ reach: 'dead', last_status: 'ok' }, 'resolves but answers nothing'],
    [{ reach: 'dead', last_status: 'nxdomain' }, 'does not resolve (nxdomain)'],
    [{ last_status: 'ok' }, 'has not been probed yet'],
    // Parking beats reachability: whatever answers is not the tracker.
    [{ parked: true, reach: 'live' }, 'resolves only to parking addresses'],
    // And an operator asking not to be contacted beats everything.
    [
      { bep34_denies: true, parked: true, reach: 'live' },
      'publishes a BEP 34 record naming no tracker and is no longer probed',
    ],
  ]
  for (const [input, want] of cases) {
    assert.equal(trackerState(input), want, JSON.stringify(input))
  }
})

// Nothing loaded yet is not the same as nothing to say; the page still has to
// carry the name it is about.
test('a detail page describes itself before and after its data lands', () => {
  const route = { name: 'detail', tracker: 'a.example.com' }

  const bare = pageMeta(route)
  assert.match(bare.title, /^a\.example\.com — /)
  assert.match(bare.description, /a\.example\.com/)

  const loaded = pageMeta(route, { name: 'a.example.com', reach: 'live', last_status: 'ok' })
  assert.equal(loaded.title, bare.title)
  assert.match(loaded.description, /^a\.example\.com resolves and answers the tracker protocol\./)
})
