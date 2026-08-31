<script>
  import {
    getStats, getChanges, collapseChanges, describe, fmtAgo, fmtSpan, fmtTime,
  } from './api.js'
  import { trackerPath } from './router.js'

  // A week, folded. Unfolded it is 820 rows; the server caps a request at 1000,
  // and the note below says when the window held more than that.
  const feedDays = 7
  const feedLimit = 1000

  let stats = $state(null)
  let changes = $state([])
  let error = $state(null)
  let loading = $state(true)

  // Folding is the default view. Off shows every entry, in order.
  let folded = $state(true)
  let opened = $state(new Set())

  $effect(() => {
    let cancelled = false
    Promise.all([getStats(), getChanges(feedLimit, feedDays)])
      .then(([s, c]) => {
        if (cancelled) return
        stats = s
        changes = c
      })
      .catch((e) => !cancelled && (error = e.message))
      .finally(() => !cancelled && (loading = false))
    return () => (cancelled = true)
  })

  const rows = $derived(
    folded
      ? collapseChanges(changes)
      : changes.map((c) => ({ kind: 'one', key: `c${c.id}`, change: c })),
  )
  const foldedAway = $derived(changes.length - rows.length)

  function toggle(key) {
    const next = new Set(opened)
    if (next.has(key)) next.delete(key)
    else next.add(key)
    opened = next
  }

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
    <div class="detail-head">
      <h2>Recent changes</h2>
      {#if foldedAway > 0 || !folded}
        <button class="link" onclick={() => (folded = !folded)}>
          {folded ? `show all ${changes.length}` : 'fold repeats'}
        </button>
      {/if}
    </div>
    {#if changes.length === 0}
      <p class="muted">
        Nothing recorded in the last {feedDays} days. A new database has no
        history yet; run <code>trackerd poll</code>.
      </p>
    {:else}
      <p class="sub">
        The last {feedDays} days.
        {#if folded && foldedAway > 0}
          {rows.length} of {changes.length} entries: a name that keeps changing the
          same thing is one row, opened by its count.
        {:else}
          {changes.length} entries.
        {/if}
        {#if changes.length >= feedLimit}
          The window holds more; this is the newest {feedLimit}.
        {/if}
      </p>
      <ul class="feed">
        {#each rows as row (row.key)}
          {#if row.kind === 'run'}
            {@const span = fmtSpan(row.earliest, row.latest)}
            <li>
              <time datetime={row.latest} title={fmtTime(row.latest)}>{fmtAgo(row.latest)}</time>
              <span class="sign net">~</span>
              <span class="body">
                <a href={trackerPath(row.tracker)}>{row.tracker}</a>
                <span class="what">{row.text}</span>
                <button
                  class="link count"
                  title="{row.count} entries, oldest {fmtTime(row.earliest)}"
                  onclick={() => toggle(row.key)}
                >
                  {opened.has(row.key) ? 'hide' : `${row.count}×`}
                </button>
                {#if span}<span class="muted">{span}</span>{/if}
              </span>
            </li>
            {#if opened.has(row.key)}
              {#each row.members as c (c.id)}
                {@const d = describe(c)}
                <li class="folded">
                  <time datetime={c.observed_at} title={fmtTime(c.observed_at)}>
                    {fmtAgo(c.observed_at)}
                  </time>
                  <span class="sign {d.cls}">{d.sign}</span>
                  <span class="body"><span class="what">{d.text}</span></span>
                </li>
              {/each}
            {/if}
          {:else}
            {@const d = describe(row.change)}
            <li>
              <time datetime={row.change.observed_at} title={fmtTime(row.change.observed_at)}>
                {fmtAgo(row.change.observed_at)}
              </time>
              <span class="sign {d.cls}">{d.sign}</span>
              <span class="body">
                <a href={trackerPath(row.change.tracker)}>{row.change.tracker}</a>
                <span class="what">{d.text}</span>
              </span>
            </li>
          {/if}
        {/each}
      </ul>
    {/if}
  </div>
{/if}
