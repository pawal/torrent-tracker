<script>
  import { getTrackers, fmtTime, describeNetwork, flag } from './api.js'

  let trackers = $state([])
  let error = $state(null)
  let loading = $state(true)
  let filter = $state('')

  $effect(() => {
    let cancelled = false
    getTrackers()
      .then((t) => !cancelled && (trackers = t))
      .catch((e) => !cancelled && (error = e.message))
      .finally(() => !cancelled && (loading = false))
    return () => (cancelled = true)
  })

  // A rolling family shows its prefix rather than the addresses inside it.
  const rolls = (t, family) => (t.rolling ?? []).includes(family)
  const rollTitle = 'addresses change every run; the prefix is what is tracked'

  const shown = $derived.by(() => {
    const q = filter.trim().toLowerCase()
    if (!q) return trackers
    return trackers.filter(
      (t) =>
        t.name.includes(q) ||
        t.ipv4.some((ip) => ip.includes(q)) ||
        t.ipv6.some((ip) => ip.includes(q)) ||
        (t.networks ?? []).some((n) =>
          `as${n.asn} ${n.holder ?? ''} ${n.rir ?? ''} ${n.country ?? ''}`.toLowerCase().includes(q),
        ),
    )
  })
</script>

{#if error}
  <div class="card"><p class="err">Failed to load: {error}</p></div>
{:else if loading}
  <div class="card"><p class="muted">Loading…</p></div>
{:else}
  <div class="card">
    <h2>Known trackers</h2>
    <div class="controls">
      <input type="search" bind:value={filter} placeholder="Filter by hostname or address" />
      <span class="muted">{shown.length} of {trackers.length}</span>
    </div>

    <div class="scroll">
      <table class="tight">
        <thead>
          <tr>
            <th class="col-name">Tracker</th>
            <!-- Named for what it is. This column has only ever been the
                 resolver's verdict, which reads as tracker health if you let
                 it; whether the tracker answers lives on the detail page. -->
            <th>DNS</th>
            <th>Network</th>
            <th>IPv4</th>
            <th>IPv6</th>
            <th>Checked</th>
          </tr>
        </thead>
        <tbody>
          {#each shown as t (t.id)}
            <tr>
              <td class="mono col-name">
                <!-- Clipped by CSS; the tooltip carries the full name, and the
                     detail page has it in the heading. -->
                <a href="#/t/{encodeURIComponent(t.name)}" title={t.name}>{t.name}</a>
              </td>
              <td>
                <span class="pill {t.last_status || 'unchecked'}">
                  {t.last_status || 'unchecked'}
                </span>
                {#if t.parked}
                  <span class="pill parked" title="resolves only to parking addresses">parked</span>
                {/if}
              </td>
              <td class="net-tag">
                <!-- Keyed on the whole tuple: one AS can appear twice with a
                     different country or RIR, which collides on asn alone. -->
                {#each t.networks ?? [] as n (`${n.asn}|${n.holder}|${n.rir}|${n.country}`)}
                  <span class="block" title={describeNetwork(n)}>
                    {flag(n.country)}
                    <span class="asn">{describeNetwork(n)}</span>
                    {#if n.rir}<span>· {n.rir}</span>{/if}
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
  </div>
{/if}
