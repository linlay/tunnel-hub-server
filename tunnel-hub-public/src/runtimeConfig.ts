const PUBLIC_SITE_TITLE_URL = '/runtime/public-site-title.txt';

export interface RuntimeTitleResponse {
  ok: boolean;
  status: number;
  text(): Promise<string>;
}

export type RuntimeTitleFetcher = (
  input: string,
  init: { cache: 'no-store' }
) => Promise<RuntimeTitleResponse>;

export async function applyPublicSiteTitle(
  fetcher: RuntimeTitleFetcher = window.fetch.bind(window)
): Promise<string> {
  const response = await fetcher(PUBLIC_SITE_TITLE_URL, { cache: 'no-store' });
  if (!response.ok) {
    throw new Error(`runtime title request failed with status ${response.status}`);
  }

  const title = (await response.text()).trim();
  if (!title) {
    throw new Error('runtime title is empty');
  }

  document.title = title;
  return title;
}
