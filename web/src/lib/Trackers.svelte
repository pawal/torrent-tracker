<script>
  import {
    getTrackers, fmtTime, fmtPercent, fmtSince, describeNetwork, flag, inCountry,
    trackerClass, classReason, healthOf, healthLabels, sortTrackers,
  } from './api.js'
  import { trackerPath } from './router.js'

  // country arrives from the URL, set by clicking a row on the networks page.
  let { country = '' } = $props()

  // The window uptime is measured over. The server does the measuring, so
  // changing it is a refetch.
  const windows = [7, 30, 90]
  let days = $state(30)

  let trackers = $state([])
  let error = $state(null)
  let loading = $state(true)
  let filter = $state('')
  let health = $state('')
  let sort = $state({ key: 'health', dir: 1 })

  $effect(() => {
    let cancelled = false
    loading = true
    // Retired names are asked for too: they belong under "Not trackers" rather
    // than nowhere, which is where a reader goes looking for a name that used
    // to be on the list.
    getTrackers(days, true)
      .then((t) => !cancelled && (trackers = t))
      .catch((e) => !cancelled && (error = e.message))
      .finally(() => !cancelled && (loading = false))
    return () => (cancelled = true)
  })

  // A rolling family shows its prefix rather than the addresses inside it.
  const rolls = (t, family) => (t.rolling ?? []).includes(family)
  const rollTitle = 'addresses change every run; the prefix is what is tracked'

  // An exact match on the code, not a search: "SE" as free text hits every
  // holder with "se" in its name.
  const inScope = $derived(
    country ? trackers.filter((t) => inCountry(t, country)) : trackers,
  )

  const matching = $derived.by(() => {
    const q = filter.trim().toLowerCase()
    if (!q) return inScope
    return inScope.filter(
      (t) =>
        t.name.includes(q) ||
        t.ipv4.some((ip) => ip.includes(q)) ||
        t.ipv6.some((ip) => ip.includes(q)) ||
        (t.networks ?? []).some((n) =>
          `as${n.asn} ${n.holder ?? ''} ${n.rir ?? ''} ${n.country ?? ''}`.toLowerCase().includes(q),
        ),
    )
  })

  // Trackers above, everything that is on the registry without being one below.
  const real = $derived(matching.filter((t) => trackerClass(t) === 'tracker'))
  const other = $derived(matching.filter((t) => trackerClass(t) !== 'tracker'))

  // Counts are within the country and search scope but before the health chip,
  // so picking one chip does not renumber the others.
  const counts = $derived.by(() => {
    const out = {}
    for (const t of real) out[healthOf(t)] = (out[healthOf(t)] ?? 0) + 1
    return out
  })

  const shown = $derived(
    sortTrackers(
      health ? real.filter((t) => healthOf(t) === health) : real,
      sort.key,
      sort.dir,
    ),
  )

  // Clicking the sorted column reverses it; clicking another takes its own
  // natural direction — names up, numbers and verdicts best first.
  const ascending = { name: 1, dns: 1, health: 1, uptime: -1, checked: -1 }
  function sortBy(key) {
    sort = sort.key === key ? { key, dir: -sort.dir } : { key, dir: ascending[key] ?? 1 }
  }
  const arrow = (key) => (sort.key === key ? (sort.dir === 1 ? ' ▲' : ' ▼') : '')

  // One AS is one network however many of its registries a name touches: the
  // tuple put AS13335 in twice on any tracker with an address in both.
  function networksOf(t) {
    const out = new Map()
    for (const n of t.networks ?? []) {
      const key = `${n.asn}|${n.holder}`
      const seen = out.get(key)
      if (seen) {
        if (n.country && !seen.countries.includes(n.country)) seen.countries.push(n.country)
        if (n.rir && !seen.rirs.includes(n.rir)) seen.rirs.push(n.rir)
        continue
      }
      out.set(key, {
        key,
        label: describeNetwork(n),
        countries: n.country ? [n.country] : [],
        rirs: n.rir ? [n.rir] : [],
      })
    }
    return [...out.values()]
  }

  function uptimeTitle(t) {
    if (t.uptime === null || t.uptime === undefined) {
      return `nothing measured in the last ${days} days`
    }
    const misses = t.misses ? `, ${t.misses} failed attempt${t.misses === 1 ? '' : 's'}` : ''
    return `${fmtPercent(t.uptime)} of measured time over ${days} days${misses}`
  }
</script>

{#if error}
  <div class="card"><p class="err">Failed to load: {error}</p></div>
{:else if loading && trackers.length === 0}
  <div class="card"><p class="muted">Loading…</p></div>
{:else}
  <div class="card">
    <div class="detail-head">
      <h2>Known trackers</h2>
      <div class="window-pick" title="the window uptime is measured over">
        {#each windows as d (d)}
          <button class:on={days === d} onclick={() => (days = d)}>{d}d</button>
        {/each}
      </div>
    </div>
    {#if country}
      <p class="sub">
        {#if country === 'unknown'}
          Trackers whose addresses have no country on record.
        {:else}
          Trackers with an address in <strong>{flag(country)} {country}</strong>. One served from
          several countries appears under each of them.
        {/if}
        <a href="/trackers">Show all</a>
      </p>
    {/if}

    <div class="controls">
      <input type="search" bind:value={filter} placeholder="Filter by hostname or address" />
      <span class="muted">
        {shown.length} of {real.length}
        {#if country}({trackers.length} in all){/if}
      </span>
    </div>

    <!-- Uptime is bimodal, so the useful question is not "how much" but "which
         of the three states", and flapping is the one worth a chip of its own. -->
    <div class="chips">
      <button class="pill chip" class:on={health === ''} onclick={() => (health = '')}>
        all<span class="count">{real.length}</span>
      </button>
      {#each Object.entries(healthLabels) as [key, label] (key)}
        {#if counts[key]}
          <button
            class="pill chip {key === 'answering' ? 'live' : key === 'flapping' ? 'partial' : 'dead'}"
            class:on={health === key}
            onclick={() => (health = health === key ? '' : key)}
          >
            {label}<span class="count">{counts[key]}</span>
          </button>
        {/if}
      {/each}
    </div>

    <div class="scroll">
      <table class="tight">
        <thead>
          <tr>
            <th class="col-name">
              <button class="sort" onclick={() => sortBy('name')}>Tracker{arrow('name')}</button>
            </th>
            <!-- Whether it answers leads, because it is the question the page
                 is asked. The resolver's verdict is a separate column and
                 nothing but that: it reads as tracker health if you let it. -->
            <th>
              <button class="sort" onclick={() => sortBy('health')}>Answers{arrow('health')}</button>
            </th>
            <th>
              <button class="sort" onclick={() => sortBy('uptime')}>Uptime{arrow('uptime')}</button>
            </th>
            <th>
              <button class="sort" onclick={() => sortBy('dns')}>DNS{arrow('dns')}</button>
            </th>
            <th>Network</th>
            <th>IPv4</th>
            <th>IPv6</th>
            <th>
              <button class="sort" onclick={() => sortBy('checked')}>Checked{arrow('checked')}</button>
            </th>
          </tr>
        </thead>
        <tbody>
          {#each shown as t (t.id)}
            <tr>
              <td class="mono col-name">
                <!-- Clipped by CSS; the tooltip carries the full name, and the
                     detail page has it in the heading. -->
                <a href={trackerPath(t.name)} title={t.name}>{t.name}</a>
              </td>
              <td>
                <!-- Silent since the window opened and silent since it was
                     added are different names: the second never was a tracker. -->
                {#if t.state && !t.state.answering && !t.last_live_at}
                  <span class="pill dead" title="resolves, but no probe has ever answered">
                    never answered
                  </span>
                {:else if t.state}
                  <span
                    class="pill {t.state.answering ? 'live' : 'dead'}"
                    title="{t.state.answering ? 'answering' : 'silent'} since {fmtTime(
                      t.state.since,
                    )}{t.state.clipped ? ', and before the window opened' : ''}"
                  >
                    {t.state.answering ? 'answering' : 'silent'}
                    {fmtSince(t.state)}
                  </span>
                {:else}
                  <span class="muted" title="nothing is being probed now">-</span>
                {/if}
              </td>
              <td class="mono nowrap" title={uptimeTitle(t)}>
                {fmtPercent(t.uptime)}
                <!-- 52 of the 105 answering names carry failures the interval
                     survived, so a round 100% is not the whole verdict. -->
                {#if t.misses}<span class="misses">{t.misses}✗</span>{/if}
              </td>
              <td>
                <span class="pill {t.last_status || 'unchecked'}">
                  {t.last_status || 'unchecked'}
                </span>
              </td>
              <td class="net-tag">
                {#each networksOf(t) as n (n.key)}
                  <span class="block" title="{n.label} · {n.rirs.join(', ')}">
                    {#each n.countries as cc (cc)}{flag(cc)}{/each}
                    <span class="asn">{n.label}</span>
                  </span>
                {:else}-{/each}
              </td>
              <td class="addr">
                {#each t.ipv4 as ip (ip)}<span>{ip}</span>{:else}-{/each}
                {#if rolls(t, 4)}<span class="rolling" title={rollTitle}>rolling</span>{/if}
              </td>
              <td class="addr">
                {#each t.ipv6 as ip (ip)}<span>{ip}</span>{:else}-{/each}
                {#if rolls(t, 6)}<span class="rolling" title={rollTitle}>rolling</span>{/if}
              </td>
              <td class="muted mono nowrap">
                {t.last_checked_at ? fmtTime(t.last_checked_at) : 'never'}
              </td>
            </tr>
          {/each}
        </tbody>
      </table>
    </div>
    {#if shown.length === 0}
      <p class="muted">
        No tracker matches. {#if health}<button class="link" onclick={() => (health = '')}>
            Drop the {healthLabels[health]} filter
          </button>{/if}
      </p>
    {/if}
  </div>

  {#if other.length}
    <div class="card" id="other">
      <h2>Not trackers</h2>
      <p class="sub">
        On the registry without being a tracker, so they are left out of the list above and of
        the rollups on the networks page. Their history stays, and each still has its own page.
      </p>
      <div class="scroll">
        <table class="tight">
          <thead>
            <tr>
              <th class="col-name">Name</th>
              <th>Why</th>
              <th>DNS</th>
              <th>Network</th>
              <th>Checked</th>
            </tr>
          </thead>
          <tbody>
            {#each other as t (t.id)}
              <tr>
                <td class="mono col-name">
                  <a href={trackerPath(t.name)} title={t.name}>{t.name}</a>
                </td>
                <td>
                  <span class="pill {trackerClass(t) === 'retired' ? 'unchecked' : 'parked'}">
                    {trackerClass(t)}
                  </span>
                  <span class="muted">{classReason(t)}</span>
                </td>
                <td>
                  <span class="pill {t.last_status || 'unchecked'}">
                    {t.last_status || 'unchecked'}
                  </span>
                </td>
                <td class="net-tag">
                  {#each networksOf(t) as n (n.key)}
                    <span class="block" title={n.label}>
                      {#each n.countries as cc (cc)}{flag(cc)}{/each}
                      <span class="asn">{n.label}</span>
                    </span>
                  {:else}-{/each}
                </td>
                <td class="muted mono nowrap">
                  {t.last_checked_at ? fmtTime(t.last_checked_at) : 'never'}
                </td>
              </tr>
            {/each}
          </tbody>
        </table>
      </div>
    </div>
  {/if}
{/if}
