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

  it('creates admin api keys', async () => {
    const fetchMock = vi.spyOn(globalThis, 'fetch').mockResolvedValue(
      new Response(
        JSON.stringify({
          apiKey: { id: 'apikey_1', name: 'bot', keyPrefix: 'za_abc', active: true, createdAt: 'now' },
          secret: 'za_secret'
        }),
        {
          status: 201,
          headers: { 'Content-Type': 'application/json' }
        }
      )
    );

    const response = await api.createApiKey('bot');

    expect(response.secret).toBe('za_secret');
    expect(fetchMock).toHaveBeenCalledWith(
      '/api/admin/api-keys',
      expect.objectContaining({
        method: 'POST',
        credentials: 'include',
        body: JSON.stringify({ name: 'bot' })
      })
    );
  });

  it('publishes managed services', async () => {
    const fetchMock = vi.spyOn(globalThis, 'fetch').mockResolvedValue(
      new Response(
        JSON.stringify({
          publicHost: 'auditor.tunnel-hub.zenmind.cc',
          publicUrl: 'https://auditor.tunnel-hub.zenmind.cc',
          route: {
            id: 'route_1',
            publicHost: 'auditor.tunnel-hub.zenmind.cc',
            targetUrl: 'http://127.0.0.1:3000',
            active: true,
            createdAt: 'now',
            updatedAt: 'now'
          }
        }),
        {
          status: 200,
          headers: { 'Content-Type': 'application/json' }
        }
      )
    );

    const response = await api.publishService('auditor', {
      targetUrl: 'http://127.0.0.1:3000',
      active: true
    });

    expect(response.publicHost).toBe('auditor.tunnel-hub.zenmind.cc');
    expect(fetchMock).toHaveBeenCalledWith(
      '/api/admin/services/auditor',
      expect.objectContaining({
        method: 'PUT',
        credentials: 'include',
        body: JSON.stringify({ targetUrl: 'http://127.0.0.1:3000', active: true })
      })
    );
  });
});
