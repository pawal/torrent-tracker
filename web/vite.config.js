import { defineConfig } from 'vite'
import { svelte } from '@sveltejs/vite-plugin-svelte'

export default defineConfig({
  plugins: [svelte()],
  build: {
    // Emitted into dist/ and embedded into the Go binary by embed.go.
    outDir: 'dist',
    emptyOutDir: true,
  },
  server: {
    // `trackerd serve` during development.
    proxy: {
      '/api': 'http://localhost:8080',
    },
  },
})
