// Per-route title and description. The server injects the same strings into
// the shell so crawlers and unfurlers see them without running JS; this keeps
// them right once the client starts navigating.

const SITE = 'torrent-tracker'

const NOT_FOUND = {
  title: `Not found — ${SITE}`,
  description: 'No page at this address.',
}

/** The clause a detail page opens with. Mirrors trackerState in meta.go. */
export function trackerState(t) {
  if (t?.bep34_denies) return 'publishes a BEP 34 record naming no tracker and is no longer probed'
  if (t?.parked) return 'resolves only to parking addresses'
  if (t?.reach === 'live') return 'resolves and answers the tracker protocol'
  if (t?.reach === 'partial') return 'answers on some of its addresses'
  if (t?.last_status && t.last_status !== 'ok') return `does not resolve (${t.last_status})`
  if (t?.reach === 'dead') return 'resolves but answers nothing'
  return 'has not been probed yet'
}

/** Title and description for a route. Mirrors pageMeta in internal/api/meta.go. */
export function pageMeta(route, tracker = null) {
  switch (route?.name) {
    case 'dashboard':
      return {
        title: `${SITE} — BitTorrent tracker DNS history`,
        description:
          'Which BitTorrent trackers still answer, and where they live. Hourly DNS ' +
          'collection, BEP 15 and BEP 48 probes, and an append-only feed of every change.',
      }
    case 'trackers':
      if (route.country === 'unknown') {
        return {
          title: `Trackers with no country on record — ${SITE}`,
          description:
            'BitTorrent trackers whose addresses have no country on record, with ' +
            'their DNS status, reachability and origin networks.',
        }
      }
      if (route.country) {
        return {
          title: `Trackers in ${route.country} — ${SITE}`,
          description:
            `BitTorrent trackers with an address in ${route.country}, with their ` +
            'DNS status, reachability and origin networks.',
        }
      }
      return {
        title: `Known trackers — ${SITE}`,
        description:
          'Every tracked BitTorrent tracker: DNS status, whether it answers, origin ' +
          'AS, country and the addresses it resolves to.',
      }
    case 'networks':
      return {
        title: `Networks — ${SITE}`,
        description:
          'Where the tracked BitTorrent trackers are hosted: origin AS, RIR, country ' +
          'and the tracker software behind each endpoint.',
      }
    case 'detail':
      // Before the fetch lands there is nothing to say about the name but its
      // own; the server has already served the fuller sentence.
      if (!tracker) {
        return {
          title: `${route.tracker} — ${SITE}`,
          description:
            `Address history, reachability and DNS status for the BitTorrent tracker ` +
            `${route.tracker}, collected hourly and probed every six hours.`,
        }
      }
      return {
        title: `${tracker.name} — ${SITE}`,
        description:
          `${tracker.name} ${trackerState(tracker)}. Address history, reachability ` +
          `and DNS status, collected hourly and probed every six hours.`,
      }
    default:
      return NOT_FOUND
  }
}

// Setters keyed by how each tag is found, so a missing one is simply skipped.
const setters = [
  ['meta[name="description"]', 'content', (m) => m.description],
  ['meta[property="og:title"]', 'content', (m) => m.title],
  ['meta[property="og:description"]', 'content', (m) => m.description],
  ['meta[name="twitter:title"]', 'content', (m) => m.title],
  ['meta[name="twitter:description"]', 'content', (m) => m.description],
]

/** Write a route's metadata into the document head. */
export function applyMeta(route, tracker = null, location = window.location) {
  const meta = pageMeta(route, tracker)
  document.title = meta.title

  for (const [selector, attr, pick] of setters) {
    document.head.querySelector(selector)?.setAttribute(attr, pick(meta))
  }

  // Canonical and og:url are absolute, and name the page actually shown.
  const url = location.origin + location.pathname + location.search
  const canonical = document.head.querySelector('link[rel="canonical"]')
  document.head.querySelector('meta[property="og:url"]')?.setAttribute('content', url)

  // A path that renders nothing must not claim to be canonical, and must not
  // be indexed. The server says the same for a cold load.
  const gone = route?.name === 'notfound'
  document.head
    .querySelector('meta[name="robots"]')
    ?.setAttribute('content', gone ? 'noindex, follow' : 'index, follow')
  // Dropping the href rather than the tag, so navigating back restores it.
  if (gone) canonical?.removeAttribute('href')
  else canonical?.setAttribute('href', url)
}
