<script>
  import { getTracker, describe, fmtTime, fmtDate, describeNetwork, flag } from './api.js'

  let { name } = $props()

  let data = $state(null)
  let error = $state(null)
  let loading = $state(true)

  $effect(() => {
    let cancelled = false
    loading = true
    error = null
    getTracker(name)
      .then((d) => !cancelled && (data = d))
      .catch((e) => !cancelled && (error = e.message))
      .finally(() => !cancelled && (loading = false))
    return () => (cancelled = true)
  })

  // Lay every address interval out on a shared time axis. Open intervals run
  // to "now" so a currently-live address reaches the right edge.
  const timeline = $derived.by(() => {
    const records = data?.records ?? []
    if (records.length === 0) return null

    const now = Date.now()
    const starts = records.map((r) => new Date(r.first_seen).getTime())
    const ends = records.map((r) => (r.active ? now : new Date(r.last_seen).getTime()))
    const min = Math.min(...starts)
    let max = Math.max(...ends, now)
    // A single-poll database has zero width; give it an hour so bars show.
    if (max - min < 3600_000) max = min + 3600_000
    const span = max - min

    return {
      min,
      rows: records.map((r) => {
        const s = new Date(r.first_seen).getTime()
        const e = r.active ? now : new Date(r.last_seen).getTime()
        return {
          ...r,
          left: ((s - min) / span) * 100,
          width: Math.max(((e - s) / span) * 100, 0.6),
        }
      }),
    }
  })
</script>

{#if error}
  <div class="card"><p class="err">Failed to load {name}: {error}</p></div>
{:else if loading}
  <div class="card"><p class="muted">Loading…</p></div>
{:else}
  <div class="card">
    <div class="detail-head">
      <h2 class="name">{data.name}</h2>
      <span class="pill {data.last_status || 'unchecked'}">{data.last_status || 'unchecked'}</span>
    </div>
    <p class="meta">
      source {data.source || 'unknown'}
      <span class="sep">·</span> added {fmtDate(data.created_at)}
      <span class="sep">·</span> last checked
      {data.last_checked_at ? fmtTime(data.last_checked_at) : 'never'}
      {#if !data.enabled}<span class="sep">·</span><span class="err">removed</span>{/if}
    </p>
    <p class="sub"><a href="#/">← back to changes</a></p>
  </div>

  <div class="card">
    <h2>Address history</h2>
    {#if !timeline}
      <p class="muted">No addresses have ever been recorded for this name.</p>
    {:else}
      <p class="sub">
        {fmtDate(new Date(timeline.min).toISOString())} → today. Green bars are live now.
      </p>
      <div class="timeline">
        {#each timeline.rows as r (r.id)}
          {@const n = data.info?.[r.ip]}
          <div class="track">
            <span title="IPv{r.family}">
              {r.ip}
              {#if n?.asn}<span class="net-tag">AS{n.asn} {flag(n.country)}</span>{/if}
            </span>
            <div
              class="bar-area"
              title="{fmtTime(r.first_seen)} → {r.active ? 'now' : fmtTime(r.last_seen)}"
            >
              <div class="bar" class:active={r.active} style="left:{r.left}%;width:{r.width}%"></div>
            </div>
          </div>
        {/each}
      </div>

      <div class="scroll">
        <table>
          <thead>
            <tr>
              <th>Address</th>
              <th>Network</th>
              <th>RIR</th>
              <th>Location</th>
              <th>First seen</th>
              <th>Last seen</th>
              <th>State</th>
            </tr>
          </thead>
          <tbody>
            {#each data.records as r (r.id)}
              {@const n = data.info?.[r.ip]}
              <tr>
                <td class="mono nowrap">{r.ip}</td>
                <td class="net-tag">
                  {#if n && (n.asn || n.org || n.as_name)}
                    <span class="asn">{describeNetwork(n)}</span>
                    {#if n.prefix}<span class="block">{n.prefix}</span>{/if}
                  {:else}-{/if}
                </td>
                <td class="muted mono">{n?.rir || '-'}</td>
                <td class="net-tag">
                  {#if n?.country}
                    {flag(n.country)}
                    {n.country}{#if n.city}, {n.city}{/if}
                  {:else}-{/if}
                </td>
                <td class="muted mono nowrap">{fmtTime(r.first_seen)}</td>
                <td class="muted mono nowrap">{fmtTime(r.last_seen)}</td>
                <td>
                  <span class="pill {r.active ? 'ok' : 'unchecked'}">
                    {r.active ? 'live' : 'gone'}
                  </span>
                </td>
              </tr>
            {/each}
          </tbody>
        </table>
      </div>
    {/if}
  </div>

  <div class="card">
    <h2>Change log</h2>
    {#if data.changes.length === 0}
      <p class="muted">No changes recorded.</p>
    {:else}
      <ul class="feed">
        {#each data.changes as c (c.id)}
          {@const d = describe(c)}
          <li>
            <time datetime={c.observed_at}>{fmtTime(c.observed_at)}</time>
            <span class="sign {d.cls}">{d.sign}</span>
            <span class="body"><span class="what">{d.text}</span></span>
          </li>
        {/each}
      </ul>
    {/if}
  </div>
{/if}
