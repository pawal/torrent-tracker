<script>
  import {
    getTracker, collapseChanges, describe, fmtAgo, fmtSpan, fmtTime, fmtDate,
    describeNetwork, flag, probeLanes, resolutionLane, addressLanes, axisTicks,
    fmtPercent, rollingFamilies, isChurn,
  } from './api.js'
  import { applyMeta } from './meta.js'

  let { name } = $props()

  // How far back the lanes reach; the server decides what a window contains.
  const windows = [7, 30, 90]
  let days = $state(30)

  // A rolling name retires a dozen addresses a day, so the list is capped
  // until asked for in full.
  const addressCap = 25
  let allAddresses = $state(false)
  // Churn inside a prefix is not news — the model says so, and the feed leaves
  // it out — so the address table leaves it out too until asked.
  let showChurn = $state(false)

  let data = $state(null)
  let error = $state(null)
  let missing = $state(false)
  let loading = $state(true)

  // The change log folds like the dashboard's feed: a rolling name's log is 164
  // address churn entries out of 200, all of them the CDN swapping edges.
  let folded = $state(true)
  let opened = $state(new Set())

  function toggle(key) {
    const next = new Set(opened)
    if (next.has(key)) next.delete(key)
    else next.add(key)
    opened = next
  }

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
  const software = $derived(unique((data?.probes ?? []).map((p) => p.software || p.signature)))
  const signatures = $derived(unique((data?.probes ?? []).map((p) => p.signature)))
  const mixedSoftware = $derived(software.length > 1)
  // A Server header the tracker wrote is already the software pill; the rest
  // name the front end in front of it.
  const servers = $derived(
    unique(
      (data?.probes ?? []).map((p) => {
        const server = p.server ?? ''
        return p.software && server.startsWith(p.software) ? '' : server
      }),
    ),
  )

  $effect(() => {
    let cancelled = false
    loading = true
    error = null
    missing = false
    getTracker(name, days)
      .then((d) => {
        if (cancelled) return
        data = d
        // The server said this already; keep it true across navigation.
        applyMeta({ name: 'detail', tracker: name }, d)
      })
      .catch((e) => {
        if (cancelled) return
        missing = e.status === 404
        error = e.message
      })
      .finally(() => !cancelled && (loading = false))
    return () => (cancelled = true)
  })

  // The server's window, not the browser's clock: the open interval ends at
  // the server's now.
  const axis = $derived.by(() => {
    const from = new Date(data?.probe_history_from ?? 0).getTime()
    if (!Number.isFinite(from) || from === 0) return null
    return { from, now: from + days * 86_400_000 }
  })

  const lanes = $derived(axis ? probeLanes(data, axis.from, axis.now) : [])
  const dns = $derived(axis ? resolutionLane(data, axis.from, axis.now) : null)
  const ticks = $derived(axis ? axisTicks(axis.from, axis.now) : [])
  const allLanes = $derived(axis ? addressLanes(data, axis.from, axis.now) : [])
  const rolling = $derived(rollingFamilies(data?.records))
  const churn = $derived(allLanes.filter((r) => isChurn(r, rolling)))
  const addresses = $derived(
    showChurn ? allLanes : allLanes.filter((r) => !isChurn(r, rolling)),
  )
  const shownAddresses = $derived(
    allAddresses ? addresses : addresses.slice(0, addressCap),
  )

  // Weighted by measured time, not by lane: a day-old address counts less.
  const uptime = $derived.by(() => {
    const measured = lanes.reduce((n, l) => n + l.measured, 0)
    if (measured === 0) return null
    return lanes.reduce((n, l) => n + l.live, 0) / measured
  })

  // Attempts that failed without breaking a verdict. A stretch reading live
  // throughout is not proof that every round inside it answered.
  const misses = $derived(lanes.reduce((n, l) => n + l.misses, 0))

  // The AS holder is the label; the org names whoever leases the prefix, which
  // is a different fact and only worth showing when the two disagree.
  function networkTitle(n) {
    const label = describeNetwork(n)
    if (!n.org || label.endsWith(n.org)) return label
    return `${label}\nprefix held by ${n.org}`
  }

  const logRows = $derived(
    folded
      ? collapseChanges(data?.changes ?? [])
      : (data?.changes ?? []).map((c) => ({ kind: 'one', key: `c${c.id}`, change: c })),
  )
  const logFolded = $derived((data?.changes?.length ?? 0) - logRows.length)

  function segTitle(what, seg) {
    const to = seg.open ? 'now' : fmtTime(new Date(seg.to).toISOString())
    const why = seg.reason ? ` (${seg.reason})` : ''
    const n = seg.lookups ? `\n${seg.lookups} lookup${seg.lookups === 1 ? '' : 's'}` : ''
    const m = seg.misses ? `\n${seg.misses} failed attempt${seg.misses === 1 ? '' : 's'}` : ''
    return `${what}: ${seg.result}${why}\n${fmtTime(new Date(seg.from).toISOString())} → ${to}${n}${m}`
  }
</script>

{#if missing}
  <div class="card">
    <h2>Not found</h2>
    <p class="muted">
      No tracker named <code>{name}</code> is on record. It may never have been
      imported, or the link may be mistyped.
    </p>
    <p class="sub"><a href="/trackers">Browse the tracker list</a></p>
  </div>
{:else if error}
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
      {#if data.bep34_denies}
        <span class="pill denies" title="publishes a BEP 34 record naming no tracker">denies</span>
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
    <p class="sub"><a href="/">← back to changes</a></p>
  </div>

  <div class="card">
    <div class="detail-head">
      <h2>Tracker protocol</h2>
      {#if !mixedSoftware && software[0]}
        <span class="pill guess" title="inferred from the reply: {signatures[0]}">
          {software[0]}
        </span>
      {/if}
      {#each servers as s (s)}
        <span class="pill guess" title="HTTP Server header — the front end, not the tracker">
          {s}
        </span>
      {/each}
    </div>
    {#if data.bep34 && !data.bep34_denies}
      <p class="sub">
        Advertises <code>{data.bep34}</code> in DNS. A UDP port names an endpoint
        outright; a TCP one is probed as both http and https, and whichever
        answers is adopted.
      </p>
    {/if}
    {#if data.bep34_denies}
      <p class="muted">
        <code>{data.bep34}</code> — this host publishes a BEP 34 record naming no
        tracker, which is the operator asking not to be contacted. It is no longer
        probed. The name and everything measured before stay on record.
      </p>
    {:else if endpoints.length === 0}
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
                      {p.software || p.signature || '-'}
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
                    {#if e.retired_at}
                      retired {fmtTime(e.retired_at)} — advertised in DNS and answering
                      under neither scheme, so it is no longer probed or listed
                    {:else}
                      not probed yet{data.last_status === 'ok'
                        ? ''
                        : ' (nothing resolved to probe)'}
                    {/if}
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
      <h2>History</h2>
      {#if dns?.uptime !== null && dns !== null}
        <span class="pill {dns.uptime > 0.99 ? 'ok' : dns.uptime > 0.5 ? 'nodata' : 'nxdomain'}"
              title="share of the window this name resolved">
          {fmtPercent(dns.uptime)} resolving
        </span>
      {/if}
      {#if uptime !== null}
        <span class="pill {uptime > 0.99 ? 'live' : uptime > 0.5 ? 'partial' : 'dead'}"
              title="share of measured time this name answered on any address">
          {fmtPercent(uptime)} answering
        </span>
      {/if}
      {#if misses > 0}
        <span class="pill partial"
              title="probes that failed inside those stretches, so the share above is not the whole story">
          {misses} failed {misses === 1 ? 'attempt' : 'attempts'}
        </span>
      {/if}
      <div class="window-pick">
        {#each windows as d (d)}
          <button class:on={days === d} onclick={() => (days = d)}>{d}d</button>
        {/each}
      </div>
    </div>
    {#if !dns && lanes.length === 0}
      <p class="muted">
        Nothing recorded in the last {days} days. A name added recently has no
        history yet; try a wider window.
      </p>
    {:else}
      <p class="sub">
        Resolving and answering on one axis, so an outage can be read as one or
        the other. Blank means nobody asked.
        {#if data.resolution_stats?.lookups}
          Resolution took {data.resolution_stats.median_ms} ms at the median and
          {data.resolution_stats.p95_ms} ms at the 95th percentile over
          {data.resolution_stats.lookups} lookups.
        {/if}
      </p>
      <div class="lanes">
        {#if dns}
          <p class="lane-group">resolution</p>
          <div class="lane">
            <span class="lane-name" title="the DNS status of the name as a whole">
              <span class="lane-ep">dns</span>
              {data.name}
            </span>
            <div class="band">
              {#each ticks as t, i (i)}
                <span class="tick" class:major={t.major} style="left:{t.left}%"></span>
              {/each}
              {#each dns.segments as seg, i (i)}
                <span
                  class="seg {seg.result}"
                  style="left:{seg.left}%;width:{seg.width}%"
                  title={segTitle('DNS', seg)}
                ></span>
              {/each}
            </div>
            <span class="uptime" title="share of the window this name resolved">
              {fmtPercent(dns.uptime)}
            </span>
          </div>
        {/if}
        {#if lanes.length > 0}
          <p class="lane-group">tracker protocol</p>
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
                    title={segTitle(`${lane.endpoint} ${lane.ip}`, seg)}
                  ></span>
                {/each}
              </div>
              <span class="uptime" title="{fmtPercent(lane.uptime)} of measured time">
                {fmtPercent(lane.uptime)}
              </span>
            </div>
          {/each}
        {/if}
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
      {#if dns}
        <p class="legend">
          <span class="tag">resolution</span>
          <span class="key ok"></span> ok
          <span class="key nodata"></span> no data
          <span class="key servfail"></span> servfail
          <span class="key nxdomain"></span> nxdomain
        </p>
      {/if}
      {#if lanes.length > 0}
        <p class="legend">
          <span class="tag">protocol</span>
          <span class="key live"></span> answers
          <span class="key dead"></span> does not answer
          <span class="key unknown"></span> probed, no verdict
          <span class="key none"></span> not probed
        </p>
      {/if}
    {/if}
  </div>

  <div class="card">
    <div class="detail-head">
      <h2>Address history</h2>
      {#if addresses.length > addressCap}
        <span class="pill unchecked" title="in the last {days} days">
          {addresses.length} addresses
        </span>
      {/if}
    </div>
    {#if data.records_total === 0}
      <p class="muted">No addresses have ever been recorded for this name.</p>
    {:else if allLanes.length === 0}
      <p class="muted">
        Nothing in the last {days} days. This name has
        {data.records_total} older address {data.records_total === 1 ? 'record' : 'records'};
        widen the window above to reach them.
      </p>
    {:else if addresses.length === 0}
      <!-- Every interval in the window is churn inside a prefix. Saying
           "nothing here" would be wrong, and the toggle has to stay reachable. -->
      <p class="muted">
        Every one of the {allLanes.length} intervals in this window is churn inside a prefix
        this name tracks whole.
        <button class="link" onclick={() => (showChurn = true)}>Show them anyway</button>
      </p>
    {:else}
      <p class="sub">
        The same window as the history above, live intervals first. Green is live
        now.
        {#if addresses.length > shownAddresses.length}
          Showing the {addressCap} most recent of {addresses.length}.
        {/if}
        {#if data.records_total > allLanes.length}
          {data.records_total - allLanes.length} older
          {data.records_total - allLanes.length === 1 ? 'interval' : 'intervals'} ended before
          this window opened and are not fetched; a wider window reaches them.
        {/if}
      </p>
      {#if churn.length}
        <p class="sub">
          {churn.length} retired
          {churn.length === 1 ? 'address is' : 'addresses are'} churn inside a prefix this name
          now tracks whole, so
          {churn.length === 1 ? 'it is' : 'they are'}
          {showChurn ? 'shown' : 'left out'} — the same reason the feed does not carry
          {churn.length === 1 ? 'it' : 'them'}.
          <button class="link" onclick={() => (showChurn = !showChurn)}>
            {showChurn ? 'hide the churn' : 'show it anyway'}
          </button>
        </p>
      {/if}
      <div class="lanes">
        {#each shownAddresses as r (r.key)}
          {@const n = data.info?.[r.ip]}
          <div class="lane">
            <span class="lane-name" title="IPv{r.family}">
              {r.ip}
              {#if r.is_prefix}<span class="rolling" title={rollTitle}>rolling</span>{/if}
              {#if n?.asn}<span class="net-tag">AS{n.asn} {flag(n.country)}</span>{/if}
            </span>
            <div class="band">
              {#each ticks as t, i (i)}
                <span class="tick" class:major={t.major} style="left:{t.left}%"></span>
              {/each}
              <span
                class="seg {r.active ? 'live' : 'gone'}"
                style="left:{r.left}%;width:{r.width}%"
                title="{fmtTime(r.first_seen)} → {r.active ? 'now' : fmtTime(r.last_seen)}"
              ></span>
            </div>
            <span class="uptime">{r.active ? 'live' : 'gone'}</span>
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
      {#if addresses.length > addressCap}
        <p class="sub">
          <button class="link" onclick={() => (allAddresses = !allAddresses)}>
            {allAddresses ? `show only the ${addressCap} most recent` : `show all ${addresses.length}`}
          </button>
        </p>
      {/if}

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
            {#each shownAddresses as r (r.key)}
              {@const n = data.info?.[r.ip]}
              <tr>
                <td class="mono nowrap">
                  {r.ip}
                  {#if r.is_prefix}<span class="rolling" title={rollTitle}>rolling</span>{/if}
                </td>
                <td class="net-tag">
                  {#if n && (n.asn || n.org || n.as_name)}
                    <!-- The label is the AS holder, so a prefix leased to
                         somebody else says so in the tooltip. -->
                    <span class="asn" title={networkTitle(n)}>{describeNetwork(n)}</span>
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
    <div class="detail-head">
      <h2>Change log</h2>
      {#if logFolded > 0 || !folded}
        <button class="link" onclick={() => (folded = !folded)}>
          {folded ? `show all ${data.changes.length}` : 'fold repeats'}
        </button>
      {/if}
    </div>
    {#if data.changes.length === 0}
      <p class="muted">No changes recorded.</p>
    {:else}
      {#if folded && logFolded > 0}
        <p class="sub">
          {logRows.length} of {data.changes.length} entries: the same thing changing
          over and over is one row, opened by its count.
        </p>
      {/if}
      <ul class="feed">
        {#each logRows as row (row.key)}
          {#if row.kind === 'run'}
            {@const span = fmtSpan(row.earliest, row.latest)}
            <li>
              <time datetime={row.latest} title={fmtTime(row.latest)}>{fmtAgo(row.latest)}</time>
              <span class="sign net">~</span>
              <span class="body">
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
              <span class="body"><span class="what">{d.text}</span></span>
            </li>
          {/if}
        {/each}
      </ul>
    {/if}
  </div>
{/if}
