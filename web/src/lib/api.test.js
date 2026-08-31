import { test } from 'node:test'
import assert from 'node:assert/strict'

import { describeNetwork } from './api.js'

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
