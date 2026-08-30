<script>
  import {
    getNetworks,
    describeEvidence,
    softwareTitle,
    describeNetwork,
    flag,
  } from './api.js'
  import { trackerPath, countryPath } from './router.js'

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
    {:else}
      <!-- Two ways to be unknown, and only one of them is about the endpoint. -->
      {#if data.probes.with_endpoints < data.probes.trackers}
        <p class="muted">
          {data.probes.trackers - data.probes.with_endpoints} names were added without an
          announce endpoint and cannot be probed; they count as unknown.
        </p>
      {/if}
      {#if data.probes.never_resolved > 0}
        <p class="muted">
          {data.probes.never_resolved} have never resolved to an address at all, endpoint or
          not, so there has never been anything to probe. They are retried daily and retired
          after a month, history kept.
        </p>
      {/if}
    {/if}
  </div>

  {#if data.software.length}
    <div class="card">
      <h2>By tracker software</h2>
      <p class="sub">
        Fingerprinted for {data.probes.fingerprinted} of {data.probes.trackers} trackers, and
        {data.probes.named} of those left a fingerprint that names the software. A failure text
        is a literal from an implementation, but the generic ones were copied between projects,
        so most only gather trackers that answer alike; a reply shape is only the keys the
        answer carried, grouped past the ones that come and go with the peers a tracker has to
        report. The <code>Server</code> header is not counted: it names the front end, and is
        <code>cloudflare</code> on most live probes. UDP endpoints disclose nothing, so this
        covers the HTTP ones.
      </p>
      <div class="scroll">
        <table>
          <thead>
            <tr><th>Software</th><th>Evidence</th><th>Trackers</th><th>Endpoints</th></tr>
          </thead>
          <tbody>
            {#each data.software as s (s.kind + s.signature)}
              <tr>
                <td title={softwareTitle(s)}>{s.name || s.signature}</td>
                <td>
                  {#if describeEvidence(s.kind)}
                    <span class="pill guess">{describeEvidence(s.kind)}</span>
                  {/if}
                </td>
                <td class="mono">{s.trackers}</td>
                <td class="mono">{s.endpoints}</td>
              </tr>
            {/each}
          </tbody>
        </table>
      </div>
    </div>
  {/if}

  {#if data.shared?.length}
    <div class="card">
      <h2>Shared addresses</h2>
      <p class="sub">
        Names answering on one address. One host means one operator and one
        outage however different the names look; a CDN edge means only one front
        end, which is what the network beside it tells you. Counted over the last
        two days, so a host handing out a rotating subset still matches. Parked
        names are left out: they share their parking address by definition.
      </p>
      <div class="scroll">
        <table class="tight">
          <thead>
            <tr>
              <th>Address</th>
              <th>Names</th>
              <th>Network</th>
              <th>Trackers</th>
            </tr>
          </thead>
          <tbody>
            {#each data.shared as a (a.ip)}
              <tr>
                <td class="mono nowrap">
                  {a.ip}
                  {#if !a.active}
                    <span class="pill parked" title="seen inside the window, not resolving there now"
                      >was</span
                    >
                  {/if}
                </td>
                <td>{a.trackers.length}</td>
                <td class="net-tag">
                  {#if a.network.asn}
                    <span class="block" title={describeNetwork(a.network)}>
                      {flag(a.network.country)}
                      <span class="asn">{describeNetwork(a.network)}</span>
                    </span>
                  {:else}-{/if}
                </td>
                <td class="mono">
                  {#each a.trackers as name (name)}
                    <a class="block" href={trackerPath(name)}>{name}</a>
                  {/each}
                </td>
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
        <p class="sub">Pick a country to list the trackers served from it.</p>
        <table>
          <thead><tr><th>Country</th><th>Trackers</th><th>Addresses</th></tr></thead>
          <tbody>
            {#each data.countries as c (c.key)}
              <tr>
                <td class="mono">
                  <a href={countryPath(c.key)}>{flag(c.key)} {c.key}</a>
                </td>
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
