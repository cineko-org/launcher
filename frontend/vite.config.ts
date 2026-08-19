import { copyFileSync } from 'node:fs';
import { resolve } from 'node:path';
import react from '@vitejs/plugin-react';
import { defineConfig } from 'vite';

const outputDirectory = '../internal/launcher/assets';

export default defineConfig({
  base: './',
  plugins: [
    react(),
    {
      name: 'copy-launcher-font',
      closeBundle() {
        copyFileSync(
          resolve('node_modules/pretendard/dist/web/variable/woff2/PretendardVariable.woff2'),
          resolve(outputDirectory, 'PretendardVariable.woff2'),
        );
      },
    },
  ],
  build: {
    outDir: outputDirectory,
    emptyOutDir: true,
    rollupOptions: {
      input: resolve('index.html'),
      output: {
        entryFileNames: 'launcher.js',
        chunkFileNames: 'launcher-[name].js',
        assetFileNames: (asset) => asset.names.some((name) => name.endsWith('.css')) ? 'launcher.css' : 'launcher-[name][extname]',
      },
    },
  },
});
