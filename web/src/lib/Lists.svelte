<script>
  import { getList } from './api.js'

  // What a client is actually asking for, in the order a reader should try
  // them. The API takes the same terms apart as parameters; these are the ones
  // worth a button.
  const presets = [
    {
      key: 'stable',
      label: 'Stable',
      note: 'Uptime of 95% or better, tracked for at least 10 days, answering now. The one worth recommending.',
    },
    {
      key: 'live',
      label: 'Answering now',
      note: 'Every endpoint that answers right now, however new and however much it flaps.',
    },
    { key: 'udp', label: 'UDP only', note: 'The stable list, UDP endpoints only.' },
    { key: 'http', label: 'HTTP and HTTPS', note: 'The stable list, HTTP and HTTPS endpoints only.' },
    {
      key: 'all',
      label: 'Everything',
      note: 'Every endpoint on record, dead or alive. For archiving, not for announcing.',
    },
  ]

  let preset = $state('stable')
  let days = $state(30)
  let perAS = $state(0)
  let minAge = $state(null)
  let wantV4 = $state(true)
  let wantV6 = $state(true)

  let body = $state('')
  let error = $state(null)
  let loading = $state(true)
  let copied = $state(false)

  // per_as spends the placement data: announcing to forty names behind one CDN
  // is a single failure domain wearing forty hats.
  const asCaps = [
    { value: 0, label: 'no cap' },
    { value: 1, label: '1 per AS' },
    { value: 2, label: '2 per AS' },
    { value: 3, label: '3 per AS' },
  ]

  const params = $derived.by(() => {
    const p = new URLSearchParams()
    if (days !== 30) p.set('days', days)
    if (perAS > 0) p.set('per_as', perAS)
    if (minAge !== null) p.set('min_age_days', minAge)
    if (!wantV4) p.set('include_ipv4_only_trackers', 'false')
    if (!wantV6) p.set('include_ipv6_only_trackers', 'false')
    return p.toString()
  })
  const path = $derived(`/api/list/${preset}${params ? `?${params}` : ''}`)

  $effect(() => {
    let cancelled = false
    const url = path
    loading = true
    error = null
    getList(url)
      .then((text) => !cancelled && (body = text))
      .catch((e) => !cancelled && (error = e.message))
      .finally(() => !cancelled && (loading = false))
    return () => (cancelled = true)
  })

  // A body can lead with '#' comment lines saying that the age floor was
  // relaxed, or why the list came back empty. Clients ignore them; here they
  // are the explanation, so they are pulled out and shown as one.
  const parsed = $derived.by(() => {
    const notes = []
    const urls = []
    for (const line of body.split('\n')) {
      const s = line.trim()
      if (!s) continue
      if (s.startsWith('#')) notes.push(s.slice(1).trim())
      else urls.push(s)
    }
    return { notes, urls }
  })

  const chosen = $derived(presets.find((p) => p.key === preset))

  async function copy() {
    try {
      await navigator.clipboard.writeText(parsed.urls.join('\n\n') + '\n')
      copied = true
      setTimeout(() => (copied = false), 1500)
    } catch {
      // No clipboard permission: the textarea is selectable, which is the
      // fallback every browser has.
      copied = false
    }
  }
</script>

<div class="card">
  <h2>Announce lists</h2>
  <p class="sub">
    The announce URLs worth pasting into a client, chosen from what has actually
    answered. Parked names, names publishing a BEP 34 record that denies
    BitTorrent, and names that have never once answered are never on a list,
    however well they resolve.
  </p>

  <div class="chips">
    {#each presets as p (p.key)}
      <button class="pill chip" class:on={preset === p.key} onclick={() => (preset = p.key)}>
        {p.label}
      </button>
    {/each}
  </div>
  <p class="sub">{chosen.note}</p>

  <div class="controls list-controls">
    <label>
      Uptime window
      <select bind:value={days}>
        {#each [7, 30, 90] as d (d)}<option value={d}>{d} days</option>{/each}
      </select>
    </label>
    <label title="most trackers to take from any one origin AS">
      Concentration
      <select bind:value={perAS}>
        {#each asCaps as c (c.value)}<option value={c.value}>{c.label}</option>{/each}
      </select>
    </label>
    <label title="how long a name must have been tracked">
      History
      <select bind:value={minAge}>
        <option value={null}>the list's own floor</option>
        {#each [0, 10, 30] as d (d)}<option value={d}>{d} days</option>{/each}
      </select>
    </label>
    <label class="check">
      <input type="checkbox" bind:checked={wantV6} />
      keep IPv4-only trackers
    </label>
    <label class="check">
      <input type="checkbox" bind:checked={wantV4} />
      keep IPv6-only trackers
    </label>
  </div>

  {#if error}
    <p class="err">Failed to load: {error}</p>
  {:else}
    {#each parsed.notes as note (note)}
      <p class="muted">{note}</p>
    {/each}

    <div class="detail-head">
      <span class="pill {parsed.urls.length ? 'live' : 'dead'}">
        {parsed.urls.length}<span class="count">
          {parsed.urls.length === 1 ? 'announce URL' : 'announce URLs'}
        </span>
      </span>
      {#if parsed.urls.length}
        <button class="link" onclick={copy}>{copied ? 'copied' : 'copy to clipboard'}</button>
      {/if}
      {#if loading}<span class="muted">loading…</span>{/if}
    </div>

    {#if parsed.urls.length}
      <!-- Readonly rather than a <pre>: a textarea is selectable with one
           keystroke, which is what someone without clipboard permission needs. -->
      <textarea class="list-body" readonly rows="14" value={parsed.urls.join('\n')}></textarea>
    {/if}

    <p class="sub">
      Served as plain text at <a href={path}>{path}</a>, one URL per entry with a
      blank line after it, so the body pastes straight into a client's tracker
      box. Every parameter above is a query parameter there.
    </p>
  {/if}
</div>

<div class="card">
  <h2>How a list is chosen</h2>
  <p class="sub">
    <strong>Uptime is the share of measured time the name answered</strong>, not
    the share of checks: probes are hours apart and irregular, so counting them
    would make two trackers on different schedules incomparable. A name nothing
    has ever spoken to has no uptime rather than 0%, and is never recommended
    however long it has resolved.
  </p>
  <p class="sub">
    <strong>Entries are endpoints, not names</strong>, because one hostname can
    be live on <code>http:2095</code> and dead on <code>https:443</code>. Lanes
    are unioned per endpoint: a name with four addresses of which one is stale is
    up, not 75% up.
  </p>
  <p class="sub">
    <strong>A list with an uptime bar also requires an answer now.</strong>
    Uptime is a claim about the window; a client is pasting the list today.
  </p>
  <p class="sub">
    <strong>Concentration is a real failure mode.</strong> Announcing to forty
    names behind one CDN is a single failure domain wearing forty hats, so the
    cap takes at most that many trackers from any one origin AS, best uptime
    first. A tracker whose network is unknown is never dropped, and the endpoints
    of one hostname count once.
  </p>
</div>
