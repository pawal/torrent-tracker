<script>
  // Three states are possible in CSS, but the button only toggles between an
  // explicit light and dark; unset simply follows the OS until first click.
  let theme = $state(localStorage.getItem('theme') ?? '')

  $effect(() => {
    const root = document.documentElement
    if (theme === 'light' || theme === 'dark') {
      root.dataset.theme = theme
      localStorage.setItem('theme', theme)
    } else {
      delete root.dataset.theme
      localStorage.removeItem('theme')
    }
  })

  function prefersDark() {
    return window.matchMedia('(prefers-color-scheme: dark)').matches
  }

  const isDark = $derived(theme === 'dark' || (theme === '' && prefersDark()))

  function toggle() {
    theme = isDark ? 'light' : 'dark'
  }
</script>

<button
  class="theme-toggle"
  onclick={toggle}
  title="Switch to {isDark ? 'light' : 'dark'} theme"
  aria-label="Switch to {isDark ? 'light' : 'dark'} theme"
>
  {isDark ? '☀' : '☾'}
</button>
