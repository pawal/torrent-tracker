<script>
  // The server sent a 404 for this path; this is what it renders.
  let { path = '' } = $props()
</script>

<div class="card">
  <h2>Not found</h2>
  <p class="muted">
    {#if path}<code>{path}</code> is not a page here.{:else}No page at this address.{/if}
    The site has three: the change feed, the tracker list and the networks
    summary.
  </p>
  <p class="sub">
    <a href="/">Changes</a>
    <span class="sep">·</span>
    <a href="/trackers">Trackers</a>
    <span class="sep">·</span>
    <a href="/networks">Networks</a>
  </p>
</div>
