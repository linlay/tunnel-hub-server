import { mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';
import { describe, expect, it } from 'vitest';
import { injectPublicSiteTitle, loadBrandConfig } from './brandConfig';

const fixtures = resolve(import.meta.dirname, '..', 'configs', 'testdata');
const validFixture = readFileSync(resolve(fixtures, 'brand.valid.yaml'), 'utf8');

function loadBrandText(contents: string) {
  const directory = mkdtempSync(join(tmpdir(), 'tunnel-hub-public-brand-'));
  const path = join(directory, 'brand.yaml');
  writeFileSync(path, contents, 'utf8');
  try {
    return loadBrandConfig(path);
  } finally {
    rmSync(directory, { recursive: true, force: true });
  }
}

describe('brand config', () => {
  it('loads the shared valid fixture', () => {
    const brand = loadBrandConfig(resolve(fixtures, 'brand.valid.yaml'));
    expect(brand.brand.id).toBe('fixture-brand');
    expect(brand.domains.desktopPublicBase).toBe('m.fixture.example.test');
  });

  it.each(['brand.invalid-unknown.yaml', 'brand.invalid-domain.yaml'])('rejects shared invalid fixture %s', (name) => {
    expect(() => loadBrandConfig(resolve(fixtures, name))).toThrow();
  });

  it('escapes a complete title before injecting it', () => {
    expect(injectPublicSiteTitle('<title>placeholder</title>', 'Fixture <Site> & "Friends"')).toBe(
      '<title>Fixture &lt;Site&gt; &amp; &quot;Friends&quot;</title>'
    );
  });

  it.each([
    ['localhost', 'localhost'],
    ['127.0.0.1', '127.0.0.1'],
    ['[::1]', '[::1]']
  ])('accepts canonical loopback endpoint host %s', (relayHost, shareHost) => {
    const contents = validFixture
      .replace('relayPublicUrl: wss://hub.fixture.example.test/tunnel', `relayPublicUrl: ws://${relayHost}:18181/tunnel`)
      .replace('sharePublicBaseUrl: https://share.fixture.example.test', `sharePublicBaseUrl: http://${shareHost}:18080`);
    const brand = loadBrandText(contents);
    expect(brand.endpoints.relayPublicUrl).toBe(`ws://${relayHost}:18181/tunnel`);
    expect(brand.endpoints.sharePublicBaseUrl).toBe(`http://${shareHost}:18080`);
  });

  it.each(['127.0.0.2', 'demo.localhost', '0.0.0.0'])('rejects reserved non-canonical local endpoint host %s', (host) => {
    expect(() => loadBrandText(validFixture.replace(
      'relayPublicUrl: wss://hub.fixture.example.test/tunnel',
      `relayPublicUrl: wss://${host}:18181/tunnel`
    ))).toThrow(/reserved non-canonical local/);
    expect(() => loadBrandText(validFixture.replace(
      'sharePublicBaseUrl: https://share.fixture.example.test',
      `sharePublicBaseUrl: https://${host}:18080`
    ))).toThrow(/reserved non-canonical local/);
  });
});
