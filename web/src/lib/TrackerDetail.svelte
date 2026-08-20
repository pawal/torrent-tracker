<script>
  import {
    getTracker, describe, fmtTime, fmtDate, describeNetwork, describeSoftware, flag,
    probeLanes, axisTicks, fmtPercent,
  } from './api.js'

  let { name } = $props()

  // How far back the reachability lanes reach. Refetching on a change keeps
  // the server the one place that decides what the window contains.
  const windows = [7, 30, 90]
  let days = $state(30)

  let data = $state(null)
  let error = $state(null)
  let loading = $state(true)

  const rollTitle = 'addresses change every run; the prefix is what is tracked'

  // Reachability is rolled up the same way the collector does it: unknown
  // results abstain rather than counting against the tracker.
  function rollUp(probes) {
    const live = probes.filter((p) => p.result === 'live').length
    const dead = probes.filter((p) => p.result === 'dead').length
    if (live && dead) return 'partial'
    if (live) return 'live'
    if (dead) return 'dead'
    return 'unknown'
  }

  const endpoints = $derived.by(() => {
    const probes = data?.probes ?? []
    return (data?.endpoints ?? []).map((e) => {
      const own = probes.filter((p) => p.endpoint_id === e.id)
      return { ...e, probes: own, reach: rollUp(own) }
    })
  })

  const unique = (xs) => [...new Set(xs.filter(Boolean))]

  // Every endpoint of a healthy tracker reports the same software, so it is
  // stated once. Disagreement is the finding, and only then goes per row.
  const software = $derived(unique((data?.probes ?? []).map((p) => p.signature)))
  const servers = $derived(unique((data?.probes ?? []).map((p) => p.server)))
  const mixedSoftware = $derived(software.length > 1)

  $effect(() => {
    let cancelled = false
    loading = true
    error = null
    getTracker(name, days)
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

  // The axis is the server's window, not the browser's clock: the open probe
  // interval was closed against the server's idea of now.
  const axis = $derived.by(() => {
    const from = new Date(data?.probe_history_from ?? 0).getTime()
    if (!Number.isFinite(from) || from === 0) return null
    return { from, now: from + days * 86_400_000 }
  })

  const lanes = $derived(axis ? probeLanes(data, axis.from, axis.now) : [])
  const ticks = $derived(axis ? axisTicks(axis.from, axis.now) : [])

  // Uptime across the whole name, weighted by measured time rather than by
  // lane, so an address probed for one day does not count as much as one
  // probed all month.
  const uptime = $derived.by(() => {
    const measured = lanes.reduce((n, l) => n + l.measured, 0)
    if (measured === 0) return null
    return lanes.reduce((n, l) => n + l.live, 0) / measured
  })

  function segTitle(lane, seg) {
    const to = seg.open ? 'now' : fmtTime(new Date(seg.to).toISOString())
    const why = seg.reason ? ` (${seg.reason})` : ''
    return `${lane.endpoint} ${lane.ip}: ${seg.result}${why}\n${fmtTime(new Date(seg.from).toISOString())} → ${to}`
  }
</script>

{#if error}
  <div class="card"><p class="err">Failed to load {name}: {error}</p></div>
{:else if loading}
  <div class="card"><p class="muted">Loading…</p></div>
{:else}
  <div class="card">
    <div class="detail-head">
      <h2 class="name">{data.name}</h2>
      <span class="pill {data.reach || 'unchecked'}" title="whether the tracker protocol answers">
        {data.reach || 'unprobed'}
      </span>
      <span class="pill {data.last_status || 'unchecked'}" title="whether the name resolves">
        DNS {data.last_status || 'unchecked'}
      </span>
      {#if data.parked}
        <span class="pill parked" title="resolves only to parking addresses">parked</span>
      {/if}
    </div>
    <p class="meta">
      source {data.source || 'unknown'}
      <span class="sep">·</span> added {fmtDate(data.created_at)}
      <span class="sep">·</span> resolved
      {data.last_checked_at ? fmtTime(data.last_checked_at) : 'never'}
      <span class="sep">·</span> probed
      {data.reach_checked_at ? fmtTime(data.reach_checked_at) : 'never'}
      {#if !data.enabled}<span class="sep">·</span><span class="err">removed</span>{/if}
    </p>
    <p class="sub"><a href="#/">← back to changes</a></p>
  </div>

  <div class="card">
    <div class="detail-head">
      <h2>Tracker protocol</h2>
      {#if !mixedSoftware && software[0]}
        <span class="pill guess" title="inferred from the reply: {software[0]}">
          {describeSoftware(software[0])}
        </span>
      {/if}
      {#each servers as s (s)}
        <span class="pill guess" title="HTTP Server header — the front end, not the tracker">
          {s}
        </span>
      {/each}
    </div>
    {#if endpoints.length === 0}
      <p class="muted">
        No announce endpoint on record, so there is nothing to speak to. This name
        was added bare; re-import the list it came from to pick its endpoints up.
      </p>
    {:else}
      <p class="sub">
        Whether the tracker answers, checked per address. A name can resolve
        perfectly and answer nothing, which is why this is not the DNS status.
      </p>
      <div class="scroll">
        <table>
          <thead>
            <tr>
              <th>Endpoint</th>
              <th>Address</th>
              <th>Answers</th>
              {#if mixedSoftware}<th>Software</th>{/if}
              <th>Detail</th>
              <th>Since</th>
            </tr>
          </thead>
          <tbody>
            {#each endpoints as e (e.id)}
              {#each e.probes as p (p.ip)}
                <tr>
                  <td class="mono nowrap" title="{e.scheme}://{data.name}:{e.port}{e.path}">
                    {e.scheme}:{e.port}
                  </td>
                  <td class="mono nowrap">{p.ip}</td>
                  <td><span class="pill {p.result}">{p.result}</span></td>
                  {#if mixedSoftware}
                    <td class="muted" title={p.signature}>
                      {describeSoftware(p.signature) || '-'}
                    </td>
                  {/if}
                  <td class="muted">
                    {p.reason || (p.rtt_ms ? `${p.rtt_ms} ms` : '-')}
                  </td>
                  <td class="muted mono nowrap">{fmtTime(p.since)}</td>
                </tr>
              {:else}
                <tr>
                  <td class="mono nowrap">{e.scheme}:{e.port}</td>
                  <td colspan={mixedSoftware ? 5 : 4} class="muted">
                    not probed yet{data.last_status === 'ok' ? '' : ' (nothing resolved to probe)'}
                  </td>
                </tr>
              {/each}
            {/each}
          </tbody>
        </table>
      </div>
    {/if}
  </div>

  <div class="card">
    <div class="detail-head">
      <h2>Reachability history</h2>
      {#if uptime !== null}
        <span class="pill {uptime > 0.99 ? 'live' : uptime > 0.5 ? 'partial' : 'dead'}"
              title="share of measured time this name answered on any address">
          {fmtPercent(uptime)} answering
        </span>
      {/if}
      <div class="window-pick">
        {#each windows as d (d)}
          <button class:on={days === d} onclick={() => (days = d)}>{d}d</button>
        {/each}
      </div>
    </div>
    {#if lanes.length === 0}
      <p class="muted">
        Nothing probed in the last {days} days. Verdicts are recorded from the
        first probe onwards, so a name added recently has no history yet.
      </p>
    {:else}
      <p class="sub">
        One lane per endpoint and address. Blank means nobody asked: probing
        starts when a name is added and stops when its address goes away.
      </p>
      <div class="lanes">
        {#each lanes as lane (lane.key)}
          <div class="lane">
            <span class="lane-name" title="IPv{lane.family} on {lane.endpoint}">
              <span class="lane-ep">{lane.endpoint}</span>
              {lane.ip}
              {#if lane.gone}<span class="rolling" title="no longer resolves, so no longer probed">gone</span>{/if}
            </span>
            <div class="band">
              {#each ticks as t, i (i)}
                <span class="tick" class:major={t.major} style="left:{t.left}%"></span>
              {/each}
              {#each lane.segments as seg, i (i)}
                <span
                  class="seg {seg.result}"
                  style="left:{seg.left}%;width:{seg.width}%"
                  title={segTitle(lane, seg)}
                ></span>
              {/each}
            </div>
            <span class="uptime" title="{fmtPercent(lane.uptime)} of measured time">
              {fmtPercent(lane.uptime)}
            </span>
          </div>
        {/each}
        <div class="lane axis">
          <span></span>
          <div class="band ruler">
            {#each ticks.filter((t) => t.major) as t, i (i)}
              <span class="tick-label" style="left:{t.left}%">{t.label}</span>
            {/each}
          </div>
          <span></span>
        </div>
      </div>
      <p class="legend">
        <span class="key live"></span> answers
        <span class="key dead"></span> does not answer
        <span class="key unknown"></span> probed, no verdict
        <span class="key none"></span> not probed
      </p>
    {/if}
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
                <td class="mono nowrap">
                  {r.ip}
                  {#if r.is_prefix}<span class="rolling" title={rollTitle}>rolling</span>{/if}
                </td>
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
