import tailwindcss from '@tailwindcss/postcss';
import react from '@vitejs/plugin-react';
import { fileURLToPath } from 'node:url';
import { defineConfig } from 'vite';

const projectRoot = fileURLToPath(new URL('.', import.meta.url));

export default defineConfig({
  build: {
    emptyOutDir: true,
    outDir: '../../internal/mobilegatewayassets/dist',
    sourcemap: false,
  },
  css: { postcss: { plugins: [tailwindcss()] } },
  plugins: [react()],
  resolve: { alias: { '@': projectRoot } },
  server: {
    host: '127.0.0.1',
    watch:
      process.env.CODEX_SANDBOX === 'seatbelt'
        ? { useFsEvents: false, usePolling: true }
        : undefined,
  },
});
