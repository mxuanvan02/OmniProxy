import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'

// Served by OmniProxy under /admin-next/ so the legacy /admin/ UI keeps working
// untouched while this client is evaluated side by side.
export default defineConfig({
  base: '/admin-next/',
  plugins: [react(), tailwindcss()],
  // Output lands in ../webnext/dist so the Go binary can //go:embed it. embed
  // cannot reach outside its own package directory, so the artifact has to live
  // beside the Go file that embeds it.
  build: {
    outDir: '../webnext/dist',
    emptyOutDir: true,
  },
  server: {
    port: 5183,
    proxy: {
      // Dev server talks to the local OmniProxy admin API. Authentication is
      // supplied by the browser exactly as it is in the embedded production UI.
      '/admin/api': {
        target: 'http://127.0.0.1:8080',
        changeOrigin: true,
      },
    },
  },
})
