<script>
  import { getNetworks, flag } from './api.js'

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
</script>

{#if error}
  <div class="card"><p class="err">Failed to load: {error}</p></div>
{:else if loading}
  <div class="card"><p class="muted">Loading…</p></div>
{:else}
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
