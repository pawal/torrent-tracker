import { test } from 'node:test'
import assert from 'node:assert/strict'

import { canonicalPath, parseRoute, trackerPath, countryPath } from './router.js'

// A trailing slash is the same page, and the server redirects to this spelling,
// so both sides have to agree on what it collapses to.
test('canonicalPath drops a trailing slash but keeps the root', () => {
  assert.equal(canonicalPath('/'), '/')
  assert.equal(canonicalPath('/trackers'), '/trackers')
  assert.equal(canonicalPath('/trackers/'), '/trackers')
  assert.equal(canonicalPath('/t/a.example.com/'), '/t/a.example.com')
  assert.equal(canonicalPath(''), '/')
})

test('parseRoute maps the four top-level pages', () => {
  assert.deepEqual(parseRoute('/'), { name: 'dashboard' })
  assert.deepEqual(parseRoute('/trackers'), { name: 'trackers', country: '' })
  assert.deepEqual(parseRoute('/networks'), { name: 'networks' })
  assert.deepEqual(parseRoute('/lists'), { name: 'lists' })
  // The trailing-slash form still renders, so a stray link is not a dead end.
  assert.deepEqual(parseRoute('/networks/'), { name: 'networks' })
  assert.deepEqual(parseRoute('/lists/'), { name: 'lists' })
})

test('parseRoute reads the country filter off the query', () => {
  assert.deepEqual(parseRoute('/trackers', 'country=SE'), { name: 'trackers', country: 'SE' })
  // An unrelated query is not a filter, and must not become one.
  assert.deepEqual(parseRoute('/trackers', 'days=7'), { name: 'trackers', country: '' })
})

test('parseRoute decodes a tracker name', () => {
  assert.deepEqual(parseRoute('/t/open.demonii.com'), {
    name: 'detail',
    tracker: 'open.demonii.com',
  })
  // Names reach the URL percent-encoded, and have to come back out intact.
  assert.deepEqual(parseRoute(trackerPath('tracker.example.com:1337')), {
    name: 'detail',
    tracker: 'tracker.example.com:1337',
  })
})

// Anything not on the list is a 404 on both sides rather than a silent
// dashboard, which is what makes the server's status code honest.
test('parseRoute rejects paths the app does not render', () => {
  for (const path of ['/nope', '/t', '/t/', '/t/a/b', '/trackers/extra', '/api/stats']) {
    assert.equal(parseRoute(path).name, 'notfound', path)
  }
})

// decodeURIComponent throws on a lone '%'; a crawler or a typo can produce one,
// and it must not take the whole page down.
test('parseRoute survives a malformed escape', () => {
  assert.equal(parseRoute('/t/%').name, 'notfound')
  assert.equal(parseRoute('/t/%zz').name, 'notfound')
})

test('path helpers encode what they are given', () => {
  assert.equal(trackerPath('a.example.com'), '/t/a.example.com')
  assert.equal(trackerPath('a b'), '/t/a%20b')
  assert.equal(countryPath('SE'), '/trackers?country=SE')
})
