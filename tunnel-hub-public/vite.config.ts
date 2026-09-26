import type { IncomingMessage, ServerResponse } from 'node:http';
import { resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import react from '@vitejs/plugin-react';
import { defineConfig, loadEnv, type Plugin } from 'vite';

const packageDirectory = fileURLToPath(new URL('.', import.meta.url));
const runtimeTitlePath = '/runtime/public-site-title.txt';

function runtimeTitlePlugin(title: string): Plugin {
  const middleware = (request: IncomingMessage, response: ServerResponse, next: () => void) => {
    const pathname = new URL(request.url ?? '/', 'http://localhost').pathname;
    if (pathname !== runtimeTitlePath) {
      next();
      return;
    }
    if (!title) {
      response.statusCode = 503;
      response.end('PUBLIC_SITE_TITLE is required');
      return;
    }
    response.statusCode = 200;
    response.setHeader('Content-Type', 'text/plain; charset=utf-8');
    response.setHeader('Cache-Control', 'no-store');
    response.end(title);
  };

  return {
    name: 'runtime-public-site-title',
    configureServer(server) {
      server.middlewares.use(middleware);
    },
    configurePreviewServer(server) {
      server.middlewares.use(middleware);
    }
  };
}

export default defineConfig(({ command, mode }) => {
  let runtimePlugins: Plugin[] = [];
  if (command === 'serve') {
    const rootEnv = loadEnv(mode, resolve(packageDirectory, '..'), '');
    const publicSiteTitle = (process.env.PUBLIC_SITE_TITLE ?? rootEnv.PUBLIC_SITE_TITLE ?? '').trim();
    runtimePlugins = [runtimeTitlePlugin(publicSiteTitle)];
  }

  return {
    plugins: [react(), ...runtimePlugins],
    server: {
      host: '127.0.0.1',
      port: 11965
    },
    test: {
      environment: 'jsdom',
      globals: true,
      environmentOptions: {
        jsdom: {
          url: 'https://device.m.example.test/'
        }
      },
      setupFiles: './src/test/setup.ts'
    }
  };
});
