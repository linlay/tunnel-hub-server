import { act, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { App } from './App';

class FakeWebSocket {
  static instances: FakeWebSocket[] = [];
  readyState = 0;
  sent: Array<Record<string, unknown>> = [];
  private listeners = new Map<string, Array<(event: Event | MessageEvent | CloseEvent) => void>>();

  constructor(readonly url: string | URL) {
    FakeWebSocket.instances.push(this);
  }

  addEventListener(type: string, listener: (event: Event | MessageEvent | CloseEvent) => void) {
    const listeners = this.listeners.get(type) ?? [];
    listeners.push(listener);
    this.listeners.set(type, listeners);
  }

  send(data: string) {
    this.sent.push(JSON.parse(data) as Record<string, unknown>);
  }

  close() {
    this.readyState = 3;
    this.emit('close', new CloseEvent('close'));
  }

  open() {
    this.readyState = 1;
    this.emit('open', new Event('open'));
  }

  reply(index: number, data: unknown) {
    const id = String(this.sent[index]?.id ?? '');
    const type = String(this.sent[index]?.type ?? '');
    this.message({ ns: this.sent[index]?.ns ?? 'd', frame: 'response', type, id, code: 0, data });
  }

  message(payload: unknown) {
    this.emit('message', new MessageEvent('message', { data: JSON.stringify(payload) }));
  }

  private emit(type: string, event: Event | MessageEvent | CloseEvent) {
    for (const listener of this.listeners.get(type) ?? []) {
      listener(event);
    }
  }
}

describe('App', () => {
  beforeEach(() => {
    FakeWebSocket.instances = [];
    vi.stubGlobal('WebSocket', FakeWebSocket);
    window.history.replaceState(null, '', 'https://zm123.m.zenmind.cc/');
  });

  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it('asks for a token before connecting', async () => {
    const user = userEvent.setup();
    render(<App />);
    await user.click(screen.getByRole('button', { name: /^connect$/i }));
    expect(screen.getByText('Token is required.')).toBeInTheDocument();
    expect(FakeWebSocket.instances).toHaveLength(0);
  });

  it('consumes token from the URL without leaving it in history', () => {
    window.history.replaceState(null, '', 'https://zm123.m.zenmind.cc/?token=secret&view=board');
    render(<App />);
    expect(screen.getByLabelText('Desktop token')).toHaveValue('secret');
    expect(window.location.href).toBe('https://zm123.m.zenmind.cc/?view=board');
  });

  it('connects, loads board issues, and renders agents', async () => {
    const user = userEvent.setup();
    render(<App />);
    await user.type(screen.getByLabelText('Desktop token'), 'secret');
    await user.click(screen.getByRole('button', { name: /^connect$/i }));

    const socket = FakeWebSocket.instances[0];
    expect(String(socket.url)).toBe('wss://zm123.m.zenmind.cc/ws?token=secret');
    await act(async () => {
      socket.open();
    });

    await waitFor(() => expect(socket.sent[0]?.type).toBe('session.hello'));
    await act(async () => {
      socket.reply(0, { protocolVersion: 1 });
    });
    await waitFor(() => expect(socket.sent[1]?.type).toBe('event.subscribe'));
    await act(async () => {
      socket.reply(1, { types: ['snapshot.updated'] });
    });

    await waitFor(() => expect(socket.sent.some((frame) => frame.type === 'snapshot.get')).toBe(true));
    for (let index = 2; index < socket.sent.length; index += 1) {
      if (socket.sent[index].type === 'snapshot.get') {
        await act(async () => {
          socket.reply(index, {
            revision: 3,
            issues: [{ id: 'ISS-1', title: 'Ship mobile board', status: 'todo', priority: 'high' }]
          });
        });
      } else if (socket.sent[index].type === 'agent.list') {
        await act(async () => {
          socket.reply(index, { agents: [{ agentKey: 'zenmi', displayName: '小宅', role: '平台总管' }] });
        });
      } else if (socket.sent[index].type === '/api/agents') {
        await act(async () => {
          socket.reply(index, { agents: [{ key: 'coder', name: 'Coder', role: 'Engineering' }] });
        });
      }
    }

    expect(await screen.findByText('Ship mobile board')).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: /agents/i }));
    const agentPanel = screen.getByRole('heading', { name: 'Agents' }).closest('section')!;
    expect(await within(agentPanel).findByRole('option', { name: '小宅' })).toBeInTheDocument();
    expect(within(agentPanel).getByRole('option', { name: 'Coder' })).toBeInTheDocument();
  });

  it('shows agent query failures', async () => {
    const user = userEvent.setup();
    render(<App />);
    await user.click(screen.getByRole('button', { name: /agents/i }));
    await user.type(screen.getByLabelText('Desktop token'), 'secret');
    await user.click(screen.getByRole('button', { name: /^connect$/i }));
    const socket = FakeWebSocket.instances[0];
    await act(async () => {
      socket.open();
    });

    await waitFor(() => expect(socket.sent[0]?.type).toBe('session.hello'));
    await act(async () => {
      socket.reply(0, {});
    });
    await waitFor(() => expect(socket.sent[1]?.type).toBe('event.subscribe'));
    await act(async () => {
      socket.reply(1, {});
    });
    await waitFor(() => expect(socket.sent.length).toBeGreaterThanOrEqual(5));
    for (let index = 2; index < socket.sent.length; index += 1) {
      await act(async () => {
        socket.reply(index, socket.sent[index].type === 'snapshot.get' ? { issues: [] } : { agents: [] });
      });
    }

    const agentPanel = screen.getByRole('heading', { name: 'Agents' }).closest('section')!;
    await user.click(within(agentPanel).getByRole('button', { name: /send/i }));
    await waitFor(() => expect(socket.sent.some((frame) => frame.type === '/api/query')).toBe(true));
    const queryIndex = socket.sent.findIndex((frame) => frame.type === '/api/query');
    const queryFrame = socket.sent[queryIndex];
    await act(async () => {
      socket.message({
        ns: 'ap',
        frame: 'error',
        type: '/api/query',
        id: queryFrame.id,
        code: 503,
        msg: 'agent-platform is not running'
      });
    });

    expect(await screen.findByText('agent-platform is not running')).toBeInTheDocument();
  });
});
