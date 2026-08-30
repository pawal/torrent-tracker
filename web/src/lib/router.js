// Real paths, not fragments: a crawler treats '#/t/x' as the same URL as '/'.
// The Go server knows the same route set, so an unknown path 404s there too.

/** Strip a trailing slash: '/trackers/' and '/trackers' are one page. */
export function canonicalPath(pathname) {
  if (!pathname) return '/'
  return pathname.length > 1 && pathname.endsWith('/') ? pathname.slice(0, -1) : pathname
}

// decodeURIComponent throws on malformed input like '/t/%'.
function decodeName(raw) {
  try {
    return decodeURIComponent(raw)
  } catch {
    return ''
  }
}

/** The route a path and query render, or 'notfound'. */
export function parseRoute(pathname, search = '') {
  const path = canonicalPath(pathname)
  if (path === '/') return { name: 'dashboard' }
  if (path === '/trackers') {
    return { name: 'trackers', country: new URLSearchParams(search).get('country') ?? '' }
  }
  if (path === '/networks') return { name: 'networks' }

  const detail = path.match(/^\/t\/([^/]+)$/)
  if (detail) {
    const tracker = decodeName(detail[1])
    if (tracker) return { name: 'detail', tracker }
  }
  return { name: 'notfound' }
}

/** Hrefs, so links and the router agree on one spelling. */
export const trackerPath = (name) => `/t/${encodeURIComponent(name)}`
export const countryPath = (cc) => `/trackers?country=${encodeURIComponent(cc)}`

/** Where the browser is now, in the form parseRoute wants. */
export function currentLocation() {
  return window.location.pathname + window.location.search
}

/**
 * Route same-origin left-clicks in-page. Modified clicks, other buttons, new
 * tabs, downloads and external hosts are left to the browser, and so are the
 * API paths, which are not pages.
 */
export function interceptLinks(onChange) {
  const onClick = (e) => {
    if (e.defaultPrevented || e.button !== 0) return
    if (e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return

    const a = e.target.closest?.('a')
    if (!a || a.target || a.hasAttribute('download')) return
    const href = a.getAttribute('href')
    if (!href || href.startsWith('#')) return

    const url = new URL(a.href)
    if (url.origin !== window.location.origin) return
    if (url.pathname.startsWith('/api/')) return

    e.preventDefault()
    const to = url.pathname + url.search
    if (to !== currentLocation()) {
      window.history.pushState({}, '', to)
      onChange()
      window.scrollTo(0, 0)
    }
  }
  document.addEventListener('click', onClick)
  return () => document.removeEventListener('click', onClick)
}
