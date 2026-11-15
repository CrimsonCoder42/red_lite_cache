import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';
import tailwindcss from '@tailwindcss/vite';
import path from 'path';

// The admin port serves the build at /portal/ (REDLITE_PORTAL_DIR, internal/api/portal.go), so every
// asset URL is under that base. In development Vite serves the same base on 5180 and proxies /admin to
// the leader's admin port: the browser sees one origin, so the session cookie and the server's
// cross-origin check (http.CrossOriginProtection) behave as in production. changeOrigin stays off:
// the Host header must match the page's origin.
export default defineConfig({
  base: '/portal/',
  plugins: [react(), tailwindcss()],
  resolve: {
    alias: { '@': path.resolve(__dirname, './src') },
  },
  server: {
    port: 5180,
    strictPort: true,
    proxy: { '/admin': { target: 'http://127.0.0.1:8097' } },
  },
});
