import { describe, expect, it, vi } from 'vitest';
import {
  DesktopWsSession,
  consumeTokenFromURL,
  createDesktopRequest,
  desktopWsUrlFromLocation,
  normalizeAgents,
  normalizeTaskBoardSnapshot,
  redactSensitiveText
} from './desktopWsClient';

class FakeWebSocket {
  static instances: FakeWebSocket[] = [];
  readyState = 0;
  sent: unknown[] = [];
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
    this.sent.push(JSON.parse(data));
  }

  close() {
    this.readyState = 3;
    this.emit('close', new CloseEvent('close'));
  }

  open() {
    this.readyState = 1;
    this.emit('open', new Event('open'));
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

describe('desktopWsClient', () => {
  it('consumes URL tokens and returns a clean URL', () => {
    const result = consumeTokenFromURL('https://zm123.m.zenmind.cc/?token=secret&view=board');
    expect(result.token).toBe('secret');
    expect(result.cleanURL).toBe('https://zm123.m.zenmind.cc/?view=board');
  });

  it('builds same-host desktop ws URLs', () => {
    expect(desktopWsUrlFromLocation({ protocol: 'https:', host: 'zm123.m.zenmind.cc' } as Location, 'secret')).toBe(
      'wss://zm123.m.zenmind.cc/ws?token=secret'
    );
    expect(desktopWsUrlFromLocation({ protocol: 'http:', host: '127.0.0.1:11965' } as Location)).toBe(
      'ws://127.0.0.1:11965/ws'
    );
  });

  it('creates desktop request frames', () => {
    const frame = createDesktopRequest('d', 'snapshot.get', {}, 'req_1');
    expect(frame).toEqual({ ns: 'd', frame: 'request', type: 'snapshot.get', id: 'req_1', payload: {} });
  });

  it('matches request responses and dispatches pushes', async () => {
    vi.useFakeTimers();
    FakeWebSocket.instances = [];
    const session = new DesktopWsSession({
      url: 'wss://zm123.m.zenmind.cc/ws',
      token: 'secret',
      WebSocketCtor: FakeWebSocket,
      requestTimeoutMs: 1000
    });
    const pushes: unknown[] = [];
    session.onPush((frame) => pushes.push(frame));

    const opened = session.connect();
    const socket = FakeWebSocket.instances[0];
    expect(String(socket.url)).toBe('wss://zm123.m.zenmind.cc/ws?token=secret');
    socket.open();
    await opened;

    const request = session.request('d', 'snapshot.get');
    expect(socket.sent).toHaveLength(1);
    const sent = socket.sent[0] as { id: string };
    socket.message({ ns: 'd', frame: 'push', type: 'snapshot.updated', data: { revision: 2 } });
    socket.message({ ns: 'd', frame: 'response', type: 'snapshot.get', id: sent.id, code: 0, data: { issues: [] } });
    await expect(request).resolves.toMatchObject({ frame: 'response', type: 'snapshot.get' });
    expect(pushes).toHaveLength(1);
    vi.useRealTimers();
  });

  it('rejects timed out requests', async () => {
    vi.useFakeTimers();
    FakeWebSocket.instances = [];
    const session = new DesktopWsSession({
      url: 'wss://zm123.m.zenmind.cc/ws',
      token: '',
      WebSocketCtor: FakeWebSocket,
      requestTimeoutMs: 10
    });
    const opened = session.connect();
    FakeWebSocket.instances[0].open();
    await opened;
    const request = session.request('d', 'device.status');
    vi.advanceTimersByTime(11);
    await expect(request).rejects.toThrow('device.status timed out');
    vi.useRealTimers();
  });

  it('rejects timed out connections', async () => {
    vi.useFakeTimers();
    FakeWebSocket.instances = [];
    const session = new DesktopWsSession({
      url: 'wss://zm123.m.zenmind.cc/ws',
      token: '',
      WebSocketCtor: FakeWebSocket,
      connectTimeoutMs: 10
    });
    const opened = session.connect();
    vi.advanceTimersByTime(11);
    await expect(opened).rejects.toThrow('WebSocket connection timed out');
    expect(session.readyState).toBe('error');
    vi.useRealTimers();
  });

  it('normalizes snapshots and agent lists', () => {
    expect(
      normalizeTaskBoardSnapshot({
        revision: '7',
        issues: [{ id: 'ISS-1', title: 'Ship', status: 'in_progress', priority: 'high' }]
      })
    ).toMatchObject({
      revision: 7,
      issues: [{ id: 'ISS-1', title: 'Ship', status: 'in_progress', priority: 'high' }]
    });

    expect(normalizeAgents({ agents: [{ key: 'zenmi', name: '小宅', stats: { unreadCount: 2 } }] }, 'agent-platform')).toEqual([
      expect.objectContaining({ agentKey: 'zenmi', displayName: '小宅', unreadCount: 2, source: 'agent-platform' })
    ]);
  });

  it('redacts sensitive text', () => {
    expect(redactSensitiveText('wss://x/ws?token=secret {"token":"secret"}', ['secret'])).toBe(
      'wss://x/ws?token=*** {"token":"***"}'
    );
  });
});
