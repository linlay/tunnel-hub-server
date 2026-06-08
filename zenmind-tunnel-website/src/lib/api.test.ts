import { afterEach, describe, expect, it, vi } from 'vitest';
import { ApiError, api } from './api';

afterEach(() => {
  vi.restoreAllMocks();
});

describe('api client', () => {
  it('logs in with credentials and cookies', async () => {
    const fetchMock = vi.spyOn(globalThis, 'fetch').mockResolvedValue(
      new Response(JSON.stringify({ username: 'admin' }), {
        status: 200,
        headers: { 'Content-Type': 'application/json' }
      })
    );

    const response = await api.login('admin', 'secret');

    expect(response.username).toBe('admin');
    expect(fetchMock).toHaveBeenCalledWith(
      '/api/admin/login',
      expect.objectContaining({
        method: 'POST',
        credentials: 'include',
        body: JSON.stringify({ username: 'admin', password: 'secret' })
      })
    );
  });

  it('raises API errors with server messages', async () => {
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(
      new Response(JSON.stringify({ error: 'authentication required' }), {
        status: 401,
        headers: { 'Content-Type': 'application/json' }
      })
    );

    await expect(api.routes()).rejects.toEqual(new ApiError(401, 'authentication required'));
  });
});

