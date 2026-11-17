import react from '@vitejs/plugin-react';
import { defineConfig } from 'vitest/config';

// Backend Go local (cmd/api). O proxy preserva o Host para que cookies e
// redirecionamentos de login continuem apontando para o servidor de desenvolvimento.
const backend = process.env.RECORVIA_API_URL ?? 'http://localhost:8081';
const proxyTarget = { target: backend, changeOrigin: false, xfwd: true };

export default defineConfig({
  plugins: [react()],
  server: {
    port: 5173,
    strictPort: true,
    proxy: {
      '/api': proxyTarget,
      '/auth': proxyTarget,
      '/portal/access': proxyTarget,
      '/webhooks': proxyTarget,
    },
  },
  preview: { port: 4173 },
  build: {
    target: 'es2022',
    sourcemap: false,
    rollupOptions: {
      onwarn(warning, warn) {
        // Comentários /* @__PURE__ */ em posição não reconhecida dentro de dependências.
        if (warning.code === 'INVALID_ANNOTATION' && warning.id?.includes('node_modules')) return;
        warn(warning);
      },
      output: {
        manualChunks(id) {
          if (!id.includes('node_modules')) return undefined;
          if (/node_modules\/(react|react-dom|scheduler|react-router)\//.test(id)) return 'react';
          if (id.includes('@mantine/dates') || id.includes('dayjs')) return 'dates';
          if (id.includes('@mantine/') || id.includes('@floating-ui')) return 'mantine';
          if (id.includes('@tanstack/')) return 'query';
          if (/node_modules\/(react-hook-form|zod|@hookform)\//.test(id)) return 'forms';
          return undefined;
        },
      },
    },
  },
  test: {
    environment: 'jsdom',
    setupFiles: ['./src/test/setup.ts'],
    include: ['src/**/*.test.{ts,tsx}'],
    css: false,
    restoreMocks: true,
    testTimeout: 15000,
  },
});
