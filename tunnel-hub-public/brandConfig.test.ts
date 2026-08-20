import { resolve } from 'node:path';
import { describe, expect, it } from 'vitest';
import { injectPublicSiteTitle, loadBrandConfig } from './brandConfig';

const fixtures = resolve(import.meta.dirname, '..', 'configs', 'testdata');

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
});
