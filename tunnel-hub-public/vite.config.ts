import react from '@vitejs/plugin-react';
import { defineConfig } from 'vitest/config';

export default defineConfig({
  plugins: [react()],
  server: {
    host: '127.0.0.1',
    port: 11965
  },
  test: {
    environment: 'jsdom',
    environmentOptions: {
      jsdom: {
        url: 'https://zm123.m.zenmind.cc/'
      }
    },
    globals: true,
    setupFiles: './src/test/setup.ts'
  }
});
