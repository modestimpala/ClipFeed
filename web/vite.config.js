import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';
import { VitePWA } from 'vite-plugin-pwa';

export default defineConfig({
  plugins: [
    react(),
    VitePWA({
      registerType: 'autoUpdate',
      manifest: {
        name: 'ClipFeed',
        short_name: 'ClipFeed',
        description: 'Self-hosted short-form video feed',
        theme_color: '#0a0a0a',
        background_color: '#0a0a0a',
        display: 'standalone',
        orientation: 'portrait',
        start_url: '/',
        icons: [
          { src: '/icon-192.png', sizes: '192x192', type: 'image/png' },
          { src: '/icon-512.png', sizes: '512x512', type: 'image/png' }
        ]
      },
      workbox: {
        runtimeCaching: [{
          urlPattern: /^\/api\/feed/,
          handler: 'NetworkFirst',
          options: { cacheName: 'api-feed', expiration: { maxAgeSeconds: 300 } }
        }]
      }
    })
  ],
  server: {
    port: 3000,
    proxy: { '/api': 'http://localhost:8080' }
  }
});
