<script>
  import Dashboard from './lib/Dashboard.svelte'
  import Trackers from './lib/Trackers.svelte'
  import TrackerDetail from './lib/TrackerDetail.svelte'
  import Networks from './lib/Networks.svelte'
  import ThemeToggle from './lib/ThemeToggle.svelte'
  import { getVersion } from './lib/api.js'

  // Minimal hash router: '#/', '#/trackers', '#/t/<name>'.
  let hash = $state(window.location.hash || '#/')

  $effect(() => {
    const onHash = () => (hash = window.location.hash || '#/')
    window.addEventListener('hashchange', onHash)
    return () => window.removeEventListener('hashchange', onHash)
  })

  // Build versions for the footer. Reads no reactive state, so it runs once;
  // a failure just leaves the chip bare.
  let version = $state('')
  let dnsVersion = $state('')

  $effect(() => {
    getVersion()
      .then((v) => {
        version = v.version ?? ''
        dnsVersion = v.dns ?? ''
      })
      .catch(() => {})
  })

  const route = $derived.by(() => {
    const path = hash.replace(/^#/, '')
    const detail = path.match(/^\/t\/(.+)$/)
    if (detail) return { name: 'detail', tracker: decodeURIComponent(detail[1]) }
    if (path === '/trackers') return { name: 'trackers' }
    if (path === '/networks') return { name: 'networks' }
    return { name: 'dashboard' }
  })
</script>

<header class="top">
  <div class="brand">
    <h1>torrent-tracker</h1>
    <span class="tagline">tracker DNS history</span>
  </div>
  <div class="header-controls">
    <nav>
      <a href="#/" class:active={route.name === 'dashboard'}>Changes</a>
      <a href="#/trackers" class:active={route.name === 'trackers' || route.name === 'detail'}>
        Trackers
      </a>
      <a href="#/networks" class:active={route.name === 'networks'}>Networks</a>
    </nav>
    <a
      class="icon-link"
      href="https://github.com/pawal/torrent-tracker"
      target="_blank"
      rel="noopener noreferrer"
      title="Source on GitHub"
      aria-label="Source on GitHub"
    >
      <svg viewBox="0 0 16 16" width="16" height="16" fill="currentColor" aria-hidden="true">
        <path
          d="M8 0C3.58 0 0 3.58 0 8c0 3.54 2.29 6.53 5.47 7.59.4.07.55-.17.55-.38
             0-.19-.01-.82-.01-1.49-2.01.37-2.53-.49-2.69-.94-.09-.23-.48-.94-.82-1.13
             -.28-.15-.68-.52-.01-.53.63-.01 1.08.58 1.23.82.72 1.21 1.87.87 2.33.66
             .07-.52.28-.87.51-1.07-1.78-.2-3.64-.89-3.64-3.95 0-.87.31-1.59.82-2.15
             -.08-.2-.36-1.02.08-2.12 0 0 .67-.21 2.2.82a7.42 7.42 0 0 1 2-.27c.68 0
             1.36.09 2 .27 1.53-1.04 2.2-.82 2.2-.82.44 1.1.16 1.92.08 2.12.51.56.82
             1.27.82 2.15 0 3.07-1.87 3.75-3.65 3.95.29.25.54.73.54 1.48 0 1.07-.01
             1.93-.01 2.2 0 .21.15.46.55.38A8.01 8.01 0 0 0 16 8c0-4.42-3.58-8-8-8Z"
        />
      </svg>
    </a>
    <ThemeToggle />
  </div>
</header>

<main>
  {#if route.name === 'detail'}
    <TrackerDetail name={route.tracker} />
  {:else if route.name === 'trackers'}
    <Trackers />
  {:else if route.name === 'networks'}
    <Networks />
  {:else}
    <Dashboard />
  {/if}
</main>

<footer class="bottom">
  <span class="version-chip">
    <span><span class="version-name">torrent-tracker</span>{version}</span>
    {#if dnsVersion}
      <span><span class="version-name">miekg/dns</span>{dnsVersion}</span>
    {/if}
  </span>
</footer>
