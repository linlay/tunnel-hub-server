import { describe, expect, it, vi } from 'vitest';
import { applyPublicSiteTitle, type RuntimeTitleFetcher } from './runtimeConfig';

describe('applyPublicSiteTitle', () => {
  it('loads and applies the runtime title without caching', async () => {
    const fetcher: RuntimeTitleFetcher = vi.fn(async () => ({
      ok: true,
      status: 200,
      text: async () => 'Example Public Site'
    }));

    await expect(applyPublicSiteTitle(fetcher)).resolves.toBe('Example Public Site');
    expect(document.title).toBe('Example Public Site');
    expect(fetcher).toHaveBeenCalledWith('/runtime/public-site-title.txt', { cache: 'no-store' });
  });

  it('rejects an empty runtime title', async () => {
    const fetcher: RuntimeTitleFetcher = async () => ({
      ok: true,
      status: 200,
      text: async () => '   '
    });

    await expect(applyPublicSiteTitle(fetcher)).rejects.toThrow('runtime title is empty');
  });

  it('rejects an unsuccessful response', async () => {
    const fetcher: RuntimeTitleFetcher = async () => ({
      ok: false,
      status: 503,
      text: async () => ''
    });

    await expect(applyPublicSiteTitle(fetcher)).rejects.toThrow('status 503');
  });
});
