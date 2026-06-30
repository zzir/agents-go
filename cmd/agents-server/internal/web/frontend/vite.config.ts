import { defineConfig, Plugin } from 'vite'
import react from '@vitejs/plugin-react'
import path from 'path'

function katexWoff2Only(): Plugin {
  return {
    name: 'katex-woff2-only',
    enforce: 'pre',
    transform(code, id) {
      if (!id.includes('katex') || !id.endsWith('.css')) return
      return code
        .replace(/,\s*url\([^)]+\.woff\)\s*format\("woff"\)/g, '')
        .replace(/,\s*url\([^)]+\.ttf\)\s*format\("truetype"\)/g, '')
    },
  }
}

export default defineConfig({
  plugins: [react(), katexWoff2Only()],
  resolve: {
    alias: {
      '@': path.resolve(__dirname, 'src'),
    },
  },
  server: {
    proxy: {
      '/api': 'http://127.0.0.1:9527',
      '/ws': {
        target: 'http://127.0.0.1:9527',
        ws: true,
      },
    },
  },
  build: {
    outDir: 'dist',
    emptyOutDir: true,
    chunkSizeWarningLimit: 1000,
    rollupOptions: {
      output: {
        manualChunks: {
          'primer': ['@primer/react', '@primer/octicons-react'],
          'highlight': ['highlight.js'],
          'katex': ['katex'],
          'marked': ['marked'],
        },
      },
    },
  },
})
