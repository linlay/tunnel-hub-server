import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { App } from './App';

afterEach(() => {
  vi.restoreAllMocks();
});

describe('App', () => {
  it('shows login when session is anonymous and submits credentials', async () => {
    const fetchMock = vi
      .spyOn(globalThis, 'fetch')
      .mockResolvedValueOnce(json({ error: 'authentication required' }, 401))
      .mockResolvedValueOnce(json({ username: 'admin' }))
      .mockResolvedValue(json([]));

    render(<App />);

    await screen.findByRole('button', { name: /sign in/i });
    await userEvent.clear(screen.getByLabelText(/password/i));
    await userEvent.type(screen.getByLabelText(/password/i), 'admin');
    await userEvent.click(screen.getByRole('button', { name: /sign in/i }));

    await waitFor(() => expect(fetchMock).toHaveBeenCalledWith('/api/admin/login', expect.anything()));
  });

  it('renders status and route data for an authenticated session', async () => {
    vi.spyOn(globalThis, 'fetch')
      .mockResolvedValueOnce(json({ username: 'admin' }))
      .mockResolvedValueOnce(
        json([
          {
            id: 'route_1',
            publicHost: 'app.example.com',
            targetUrl: 'http://127.0.0.1:3000',
            active: true,
            createdAt: new Date().toISOString(),
            updatedAt: new Date().toISOString()
          }
        ])
      )
      .mockResolvedValueOnce(json([]))
      .mockResolvedValueOnce(json([]))
      .mockResolvedValueOnce(json([]))
      .mockResolvedValueOnce(json([]))
      .mockResolvedValueOnce(json({ hasActiveAgent: true, totalStreams: 3, activeStreams: 1 }));

    render(<App />);

    expect(await screen.findByText('app.example.com')).toBeInTheDocument();
    expect(await screen.findByText('Online')).toBeInTheDocument();
  });
});

function json(payload: unknown, status = 200) {
  return new Response(JSON.stringify(payload), {
    status,
    headers: { 'Content-Type': 'application/json' }
  });
}
