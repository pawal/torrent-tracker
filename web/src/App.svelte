<script>
  import Dashboard from './lib/Dashboard.svelte'
  import Trackers from './lib/Trackers.svelte'
  import TrackerDetail from './lib/TrackerDetail.svelte'
  import Networks from './lib/Networks.svelte'
  import ThemeToggle from './lib/ThemeToggle.svelte'

  // Minimal hash router: '#/', '#/trackers', '#/t/<name>'.
  let hash = $state(window.location.hash || '#/')

  $effect(() => {
    const onHash = () => (hash = window.location.hash || '#/')
    window.addEventListener('hashchange', onHash)
    return () => window.removeEventListener('hashchange', onHash)
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
    <span>torrent-tracker</span>
    <span>miekg/dns</span>
  </span>
</footer>
