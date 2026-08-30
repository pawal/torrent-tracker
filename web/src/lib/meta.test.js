import { test } from 'node:test'
import assert from 'node:assert/strict'

import { pageMeta } from './meta.js'
import { parseRoute } from './router.js'

const metaFor = (path, search) => pageMeta(parseRoute(path, search))

// Two pages sharing a title are two pages a search engine cannot tell apart,
// which is the whole reason the static one had to go.
test('every route has its own title', () => {
  const paths = ['/', '/trackers', '/networks', '/t/a.example.com', '/nope']
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
