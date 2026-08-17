<script>
  import { getNetworks, describeSoftware, flag } from './api.js'

  let data = $state(null)
  let error = $state(null)
  let loading = $state(true)

  $effect(() => {
    let cancelled = false
    getNetworks(25)
      .then((d) => !cancelled && (data = d))
      .catch((e) => !cancelled && (error = e.message))
      .finally(() => !cancelled && (loading = false))
    return () => (cancelled = true)
  })

  // Best first here, unlike the resolution pills: the headline is how much of
  // the registry is still alive, not what went wrong.
  const order = ['live', 'partial', 'dead', 'unknown']
  const reachEntries = $derived(
    Object.entries(data?.reach ?? {}).sort((a, b) => order.indexOf(a[0]) - order.indexOf(b[0])),
  )
  const answering = $derived((data?.reach?.live ?? 0) + (data?.reach?.partial ?? 0))
</script>

{#if error}
  <div class="card"><p class="err">Failed to load: {error}</p></div>
{:else if loading}
  <div class="card"><p class="muted">Loading…</p></div>
{:else}
  <div class="card">
    <h2>Tracker reachability</h2>
    <p class="sub">
      {answering} of {data.probes.trackers} names answer the tracker protocol, across
      {data.probes.endpoints} announce endpoints on {data.probes.with_endpoints} names.
      Resolving in DNS is a separate question, and a good deal more of them manage that.
    </p>
    {#if reachEntries.length}
      <div class="pill-row">
        {#each reachEntries as [state, n] (state)}
          <span class="pill {state}">{state}<span class="count">{n}</span></span>
        {/each}
      </div>
    {/if}
    {#if data.probes.probed === 0}
      <p class="muted">Run <code>trackerd probe</code> to populate this.</p>
    {:else if data.probes.with_endpoints < data.probes.trackers}
      <p class="muted">
        {data.probes.trackers - data.probes.with_endpoints} names were added without an
        announce endpoint and cannot be probed; they count as unknown.
      </p>
    {/if}
  </div>

  {#if data.software.length}
    <div class="card">
      <h2>By tracker software</h2>
      <p class="sub">
        Identified for {data.probes.identified} of {data.probes.trackers} trackers, from the
        failure text and reply shape each implementation hardcodes. UDP endpoints disclose
        nothing, so this covers the HTTP ones that answered.
      </p>
      <div class="scroll">
        <table>
          <thead>
            <tr><th>Software</th><th>Trackers</th><th>Endpoints</th></tr>
          </thead>
          <tbody>
            {#each data.software as s (s.signature)}
              <tr>
                <td title={s.signature}>{describeSoftware(s.signature)}</td>
                <td class="mono">{s.trackers}</td>
                <td class="mono">{s.endpoints}</td>
              </tr>
            {/each}
          </tbody>
        </table>
      </div>
    </div>
  {/if}

  <div class="card">
    <h2>Enrichment coverage</h2>
    <p class="sub">
      {data.coverage.enriched} of {data.coverage.active_ips} live addresses looked up,
      {data.coverage.with_asn} with an origin AS.
    </p>
    {#if data.coverage.enriched === 0}
      <p class="muted">Run <code>trackerd enrich</code> to populate this.</p>
    {/if}
  </div>

  {#if data.coverage.enriched > 0}
    <div class="card">
      <h2>Top networks</h2>
      <div class="scroll">
        <table>
          <thead>
            <tr><th>AS</th><th>Holder</th><th>Trackers</th><th>Addresses</th></tr>
          </thead>
          <tbody>
            {#each data.networks as n (n.key)}
              <tr>
                <td class="mono nowrap">{n.key}</td>
                <td>{n.label || '-'}</td>
                <td class="mono">{n.trackers}</td>
                <td class="mono">{n.ips}</td>
              </tr>
            {/each}
          </tbody>
        </table>
      </div>
    </div>

    <div class="two-up">
      <div class="card">
        <h2>By RIR</h2>
        <table>
          <thead><tr><th>Registry</th><th>Trackers</th><th>Addresses</th></tr></thead>
          <tbody>
            {#each data.rirs as r (r.key)}
              <tr>
                <td class="mono">{r.key}</td>
                <td class="mono">{r.trackers}</td>
                <td class="mono">{r.ips}</td>
              </tr>
            {/each}
          </tbody>
        </table>
      </div>

      <div class="card">
        <h2>By country</h2>
        <table>
          <thead><tr><th>Country</th><th>Trackers</th><th>Addresses</th></tr></thead>
          <tbody>
            {#each data.countries as c (c.key)}
              <tr>
                <td class="mono">{flag(c.key)} {c.key}</td>
                <td class="mono">{c.trackers}</td>
                <td class="mono">{c.ips}</td>
              </tr>
            {/each}
          </tbody>
        </table>
      </div>
    </div>
  {/if}
{/if}
