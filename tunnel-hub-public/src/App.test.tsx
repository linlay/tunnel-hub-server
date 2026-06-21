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
    await user.click(screen.getByRole('button', { name: '连接' }));
    expect(screen.getAllByText('需要 Desktop token').length).toBeGreaterThan(0);
    expect(FakeWebSocket.instances).toHaveLength(0);
  });

  it('consumes token from the URL without leaving it in history', async () => {
    window.history.replaceState(null, '', 'https://zm123.m.zenmind.cc/?token=secret&view=board');
    render(<App />);
    expect(screen.queryByLabelText('Desktop token')).not.toBeInTheDocument();
    expect(window.location.href).toBe('https://zm123.m.zenmind.cc/?view=board');
    await waitFor(() => expect(FakeWebSocket.instances).toHaveLength(1));
    expect(String(FakeWebSocket.instances[0].url)).toBe('wss://zm123.m.zenmind.cc/ws?token=secret');
  });

  it('connects, loads board issues, renders agents, and moves an issue', async () => {
    const user = userEvent.setup();
    render(<App />);
    await user.type(screen.getByLabelText('Desktop token'), 'secret');
    await user.click(screen.getByRole('button', { name: '连接' }));

    const socket = FakeWebSocket.instances[0];
    expect(String(socket.url)).toBe('wss://zm123.m.zenmind.cc/ws?token=secret');
    await act(async () => {
      socket.open();
    });

    await replyBootstrap(socket, {
      issues: [{ id: 'ISS-1', title: 'Ship mobile board', status: 'todo', priority: 'high' }],
      desktopAgents: [{ agentKey: 'zenmi', displayName: '小宅', role: '平台总管' }],
      platformAgents: [{ key: 'coder', name: 'Coder', role: 'Engineering' }]
    });

    const agentSelect = screen.getByRole('combobox', { name: '选择智能体' });
    expect(within(agentSelect).getByRole('option', { name: '小宅' })).toBeInTheDocument();
    expect(within(agentSelect).getByRole('option', { name: 'Coder' })).toBeInTheDocument();

    await user.click(screen.getByRole('button', { name: '看板' }));
    expect(await screen.findByText('Ship mobile board')).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: '右移 Ship mobile board' }));
    await waitFor(() => expect(socket.sent.some((frame) => frame.type === 'issue.move')).toBe(true));
    const moveIndex = socket.sent.findIndex((frame) => frame.type === 'issue.move');
    expect(socket.sent[moveIndex].payload).toMatchObject({ issueId: 'ISS-1', status: 'in_progress' });
    await act(async () => {
      socket.reply(moveIndex, { issue: { id: 'ISS-1', title: 'Ship mobile board', status: 'in_progress', priority: 'high' } });
    });
  });

  it('appends agent replies and shows agent query failures', async () => {
    const user = userEvent.setup();
    render(<App />);
    await user.type(screen.getByLabelText('Desktop token'), 'secret');
    await user.click(screen.getByRole('button', { name: '连接' }));
    const socket = FakeWebSocket.instances[0];
    await act(async () => {
      socket.open();
    });

    await replyBootstrap(socket, {
      issues: [],
      desktopAgents: [{ agentKey: 'zenmi', displayName: '小宅', role: '平台总管' }],
      platformAgents: []
    });

    await user.clear(screen.getByLabelText('Agent message'));
    await user.type(screen.getByLabelText('Agent message'), '你好');
    await user.click(screen.getByRole('button', { name: '发送' }));
    await waitFor(() => expect(socket.sent.some((frame) => frame.type === '/api/query')).toBe(true));
    const queryIndex = socket.sent.findIndex((frame) => frame.type === '/api/query');
    expect(socket.sent[queryIndex].payload).toMatchObject({ agentKey: 'zenmi', message: '你好', stream: true });
    await act(async () => {
      socket.reply(queryIndex, { answer: '你好，我在。' });
    });

    expect(await screen.findByText('你好，我在。')).toBeInTheDocument();

    await user.type(screen.getByLabelText('Agent message'), '再试一次');
    await user.click(screen.getByRole('button', { name: '发送' }));
    const failureIndex = lastFrameIndex(socket.sent, '/api/query');
    await act(async () => {
      socket.message({
        ns: 'ap',
        frame: 'error',
        type: '/api/query',
        id: socket.sent[failureIndex].id,
        code: 503,
        msg: 'agent-platform is not running'
      });
    });

    expect(await screen.findAllByText('agent-platform is not running')).not.toHaveLength(0);
  });

  it('renders agent stream reasoning and content before stream completion', async () => {
    const user = userEvent.setup();
    render(<App />);
    await user.type(screen.getByLabelText('Desktop token'), 'secret');
    await user.click(screen.getByRole('button', { name: '连接' }));
    const socket = FakeWebSocket.instances[0];
    await act(async () => {
      socket.open();
    });

    await replyBootstrap(socket, {
      issues: [],
      desktopAgents: [{ agentKey: 'zenmi', displayName: '小宅', role: '平台总管' }],
      platformAgents: []
    });

    await user.clear(screen.getByLabelText('Agent message'));
    await user.type(screen.getByLabelText('Agent message'), '流式回答');
    await user.click(screen.getByRole('button', { name: '发送' }));
    await waitFor(() => expect(socket.sent.some((frame) => frame.type === '/api/query')).toBe(true));
    const queryIndex = lastFrameIndex(socket.sent, '/api/query');
    const id = socket.sent[queryIndex].id;
    await act(async () => {
      socket.message({
        ns: 'ap',
        frame: 'stream',
        type: '/api/query',
        id,
        event: {
          type: 'reasoning.delta',
          reasoningLabel: '正在追线索',
          delta: '先检查看板。'
        }
      });
      socket.message({
        ns: 'ap',
        frame: 'stream',
        type: '/api/query',
        id,
        event: {
          type: 'content.delta',
          contentId: 'content_1',
          delta: '这是流式正文。'
        }
      });
    });

    expect(await screen.findByText('正在追线索')).toBeInTheDocument();
    expect(screen.getByText('先检查看板。')).toBeInTheDocument();
    expect(screen.getByText('这是流式正文。')).toBeInTheDocument();

    await act(async () => {
      socket.message({
        ns: 'ap',
        frame: 'stream',
        type: '/api/query',
        id,
        reason: 'done',
        lastSeq: 8
      });
    });
  });
});

async function replyBootstrap(
  socket: FakeWebSocket,
  data: {
    issues: unknown[];
    desktopAgents: unknown[];
    platformAgents: unknown[];
  }
) {
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
        socket.reply(index, { revision: 3, issues: data.issues });
      });
    } else if (socket.sent[index].type === 'agent.list') {
      await act(async () => {
        socket.reply(index, { agents: data.desktopAgents });
      });
    } else if (socket.sent[index].type === '/api/agents') {
      await act(async () => {
        socket.reply(index, { agents: data.platformAgents });
      });
    }
  }
}

function lastFrameIndex(frames: Array<Record<string, unknown>>, type: string) {
  for (let index = frames.length - 1; index >= 0; index -= 1) {
    if (frames[index].type === type) {
      return index;
    }
  }
  return -1;
}
