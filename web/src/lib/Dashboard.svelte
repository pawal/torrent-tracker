<script>
  import { getStats, getChanges, describe, fmtAgo, fmtTime } from './api.js'
  import { trackerPath } from './router.js'

  let stats = $state(null)
  let changes = $state([])
  let error = $state(null)
  let loading = $state(true)

  $effect(() => {
    let cancelled = false
    Promise.all([getStats(), getChanges(200)])
      .then(([s, c]) => {
        if (cancelled) return
        stats = s
        changes = c
      })
      .catch((e) => !cancelled && (error = e.message))
      .finally(() => !cancelled && (loading = false))
    return () => (cancelled = true)
  })

  // Worst first, so a wall of "ok" never buries the interesting statuses.
  const order = ['nxdomain', 'servfail', 'timeout', 'nodata', 'unchecked', 'ok']
  const statusEntries = $derived(
    Object.entries(stats?.by_status ?? {}).sort(
      (a, b) => order.indexOf(a[0]) - order.indexOf(b[0]),
    ),
  )
</script>

{#if error}
  <div class="card"><p class="err">Failed to load: {error}</p></div>
{:else if loading}
  <div class="card"><p class="muted">Loading…</p></div>
{:else}
  <div class="stats">
    <div class="stat">
      <span class="n">{stats.enabled_trackers}</span><span class="k">trackers tracked</span>
    </div>
    <div class="stat" title="distinct addresses; one shared by several trackers counts once">
      <span class="n">{stats.active_ips}</span><span class="k">addresses live now</span>
    </div>
    <div class="stat" title="one per tracker and address; the gap is trackers sharing a CDN">
      <span class="n">{stats.active_ip_records}</span><span class="k">tracker-address pairs</span>
    </div>
    <div class="stat">
      <span class="n">{stats.total_ips}</span><span class="k">addresses ever seen</span>
    </div>
    <div class="stat">
      <span class="n">{stats.changes}</span><span class="k">changes recorded</span>
    </div>
    {#if stats.parked}
      <div class="stat">
        <span class="n">{stats.parked}</span><span class="k">parked, not trackers</span>
      </div>
    {/if}
    {#if stats.never_answered}
      <div class="stat" title="resolves fine; no probe has ever got a tracker reply out of it">
        <span class="n">{stats.never_answered}</span><span class="k">never answered</span>
      </div>
    {/if}
    {#if stats.went_quiet}
      <div class="stat" title="answered at some point, and does not now">
        <span class="n">{stats.went_quiet}</span><span class="k">answered once, now dead</span>
      </div>
    {/if}
  </div>

  {#if statusEntries.length}
    <div class="card">
      <h2>Resolution status</h2>
      <div class="pill-row">
        {#each statusEntries as [status, n] (status)}
          <span class="pill {status}">{status}<span class="count">{n}</span></span>
        {/each}
      </div>
      {#if stats.last_run}
        <p class="sub">
          Last collection {fmtTime(stats.last_run.started_at)}:
          {stats.last_run.ok_count} resolved, {stats.last_run.error_count} failed,
          {stats.last_run.change_count} changes.
        </p>
      {/if}
    </div>
  {/if}

  <div class="card">
    <h2>Recent changes</h2>
    {#if changes.length === 0}
      <p class="muted">Nothing recorded yet. Run <code>trackerd poll</code>.</p>
    {:else}
      <ul class="feed">
        {#each changes as c (c.id)}
          {@const d = describe(c)}
          <li>
            <time datetime={c.observed_at} title={fmtTime(c.observed_at)}>{fmtAgo(c.observed_at)}</time>
            <span class="sign {d.cls}">{d.sign}</span>
            <span class="body">
              <a href={trackerPath(c.tracker)}>{c.tracker}</a>
              <span class="what">{d.text}</span>
            </span>
          </li>
        {/each}
      </ul>
    {/if}
  </div>
{/if}
