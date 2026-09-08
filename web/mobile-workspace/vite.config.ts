import tailwindcss from '@tailwindcss/postcss';
import react from '@vitejs/plugin-react';
import { fileURLToPath } from 'node:url';
import { readFileSync } from 'node:fs';
import { defineConfig } from 'vite';

const projectRoot = fileURLToPath(new URL('.', import.meta.url));

export default defineConfig({
	publicDir: false,
  build: {
    emptyOutDir: true,
    outDir: '../../internal/mobilegatewayassets/dist',
    sourcemap: false,
  },
  css: { postcss: { plugins: [tailwindcss()] } },
  plugins: [react(), { name: 'panel-public-assets', generateBundle() {
    for (const name of ['favicon.svg', 'og.png']) {
      this.emitFile({ type: 'asset', fileName: name, source: readFileSync(new URL(`./public/${name}`, import.meta.url)) });
    }
  } }],
  resolve: { alias: { '@': projectRoot } },
  server: {
    host: '127.0.0.1',
    watch:
      process.env.CODEX_SANDBOX === 'seatbelt'
        ? { useFsEvents: false, usePolling: true }
        : undefined,
  },
});
