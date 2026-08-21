import { resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import react from '@vitejs/plugin-react';
import { defineConfig, type Plugin } from 'vite';
import { injectPublicSiteTitle, loadBrandConfig } from './brandConfig';

const packageDirectory = fileURLToPath(new URL('.', import.meta.url));

export function brandTitlePlugin(title: string): Plugin {
  return {
    name: 'brand-public-site-title',
    transformIndexHtml(html) {
      return injectPublicSiteTitle(html, title);
    }
  };
}

export default defineConfig(({ mode }) => {
  const defaultFile = mode === 'test' ? '../configs/brand.example.yaml' : '../configs/brand.yaml';
  const configuredFile = process.env.BRAND_CONFIG_FILE;
  const brandFile = configuredFile
    ? resolve(process.cwd(), configuredFile)
    : resolve(packageDirectory, defaultFile);
  const brand = loadBrandConfig(brandFile);

  return {
    plugins: [react(), brandTitlePlugin(brand.brand.publicSiteTitle)],
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
