export type DesktopNamespace = 'd' | 'ap' | 'wa';
export type DesktopFrameKind = 'request' | 'response' | 'push' | 'stream' | 'error';
export type ConnectionState = 'idle' | 'connecting' | 'open' | 'closed' | 'error';

export type DesktopFrame = {
  ns?: DesktopNamespace;
  frame?: DesktopFrameKind | string;
  type?: string;
  id?: string;
  code?: number;
  msg?: string;
  payload?: unknown;
  data?: unknown;
  event?: unknown;
  [key: string]: unknown;
};

export type DesktopRequest = DesktopFrame & {
  ns: DesktopNamespace;
  frame: 'request';
  type: string;
  id: string;
  payload: unknown;
};

export type DesktopResponse = DesktopFrame & {
  ns: DesktopNamespace;
  frame: 'response' | 'error';
  type: string;
  id: string;
  code?: number;
  data?: unknown;
};

export type TaskBoardStatus = 'backlog' | 'todo' | 'in_progress' | 'in_review' | 'completed';
export type TaskBoardPriority = 'low' | 'medium' | 'high';

export type TaskBoardIssue = {
  id: string;
  title: string;
  description?: string;
  status: TaskBoardStatus;
  priority?: TaskBoardPriority;
  assigneeAgentKey?: string;
  assigneeAgentName?: string;
  runState?: string;
  updatedAt?: string;
  createdAt?: string;
  [key: string]: unknown;
};

export type TaskBoardSnapshot = {
  revision?: number;
  issues: TaskBoardIssue[];
  connectionState?: string;
  [key: string]: unknown;
};

export type AgentSummary = {
  agentKey: string;
  displayName: string;
  role?: string;
  unreadCount?: number;
  source?: 'desktop' | 'agent-platform';
  [key: string]: unknown;
};

type WebSocketLike = {
  readyState: number;
  send: (data: string) => void;
  close: (code?: number, reason?: string) => void;
  addEventListener: (type: string, listener: (event: Event | MessageEvent | CloseEvent) => void) => void;
  removeEventListener?: (type: string, listener: (event: Event | MessageEvent | CloseEvent) => void) => void;
};

type WebSocketConstructorLike = new (url: string | URL) => WebSocketLike;

export type DesktopWsSessionOptions = {
  url: string;
  token: string;
  WebSocketCtor?: WebSocketConstructorLike;
  requestTimeoutMs?: number;
  connectTimeoutMs?: number;
};

type PendingRequest = {
  resolve: (frame: DesktopResponse) => void;
  reject: (error: Error) => void;
  timer: ReturnType<typeof setTimeout>;
};

const DEFAULT_REQUEST_TIMEOUT_MS = 12_000;
const DEFAULT_CONNECT_TIMEOUT_MS = 12_000;
let nextRequestCounter = 0;

export function createRequestId(prefix = 'web') {
  nextRequestCounter += 1;
  return `${prefix}_${Date.now().toString(36)}_${nextRequestCounter.toString(36)}`;
}

export function createDesktopRequest(
  ns: DesktopNamespace,
  type: string,
  payload: unknown = {},
  id = createRequestId(ns)
): DesktopRequest {
  return {
    ns,
    frame: 'request',
    type,
    id,
    payload: payload ?? {}
  };
}

export function desktopWsUrlFromLocation(locationLike: Pick<Location, 'protocol' | 'host'>, token?: string) {
  const protocol = locationLike.protocol === 'https:' ? 'wss:' : 'ws:';
  const url = new URL(`${protocol}//${locationLike.host}/ws`);
  if (token?.trim()) {
    url.searchParams.set('token', token.trim());
  }
  return url.toString();
}

export function consumeTokenFromURL(rawURL: string) {
  const url = new URL(rawURL);
  const token = url.searchParams.get('token')?.trim() || '';
  url.searchParams.delete('token');
  return {
    token,
    cleanURL: url.toString()
  };
}

export function redactSensitiveText(value: string, secrets: string[] = []) {
  let redacted = value
    .replace(/([?&]token=)[^&\s]+/giu, '$1***')
    .replace(/(Authorization:\s*Bearer\s+)[^\s"']+/giu, '$1***')
    .replace(/("token"\s*:\s*")[^"]+"/giu, '$1***"')
    .replace(/("apiKey"\s*:\s*")[^"]+"/giu, '$1***"');

  for (const secret of secrets) {
    const trimmed = secret.trim();
    if (trimmed) {
      redacted = redacted.replaceAll(trimmed, '***');
    }
  }
  return redacted;
}

export function parseDesktopFrame(raw: unknown): DesktopFrame {
  if (typeof raw === 'string') {
    const parsed = JSON.parse(raw) as unknown;
    return parseDesktopFrame(parsed);
  }
  if (!raw || typeof raw !== 'object' || Array.isArray(raw)) {
    throw new Error('Desktop frame must be an object');
  }
  return raw as DesktopFrame;
}

export function extractResponsePayload(frame: DesktopFrame) {
  return frame.data ?? frame.payload ?? {};
}

export function normalizeTaskBoardSnapshot(value: unknown): TaskBoardSnapshot {
  const record = asRecord(value);
  const rawIssues = Array.isArray(record.issues) ? record.issues : [];
  return {
    ...record,
    revision: readNumber(record.revision),
    connectionState: readString(record.connectionState),
    issues: rawIssues
      .map(normalizeTaskBoardIssue)
      .filter((issue): issue is TaskBoardIssue => Boolean(issue))
  };
}

export function normalizeAgents(value: unknown, source: 'desktop' | 'agent-platform'): AgentSummary[] {
  const root = asRecord(value);
  const candidates = Array.isArray(root.agents)
    ? root.agents
    : Array.isArray(root.items)
      ? root.items
      : Array.isArray(root.results)
        ? root.results
        : Array.isArray(value)
          ? value
          : [];

  return candidates
    .map((item): AgentSummary | null => {
      const record = asRecord(item);
      const nestedStats = asRecord(record.stats);
      const agentKey = readString(record.agentKey) || readString(record.key) || readString(record.id);
      const displayName =
        readString(record.displayName) ||
        readString(record.name) ||
        readString(record.label) ||
        agentKey;
      if (!agentKey || !displayName) {
        return null;
      }
      const role = readString(record.role);
      return {
        ...record,
        agentKey,
        displayName,
        role: role || undefined,
        unreadCount: readNumber(record.unreadCount ?? nestedStats.unreadCount),
        source
      };
    })
    .filter((agent): agent is AgentSummary => Boolean(agent));
}

export class DesktopWsSession {
  private socket: WebSocketLike | null = null;
  private readonly pending = new Map<string, PendingRequest>();
  private readonly messageListeners = new Set<(frame: DesktopFrame) => void>();
  private readonly pushListeners = new Set<(frame: DesktopFrame) => void>();
  private readonly stateListeners = new Set<(state: ConnectionState) => void>();
  private state: ConnectionState = 'idle';

  constructor(private readonly options: DesktopWsSessionOptions) {}

  get readyState() {
    return this.state;
  }

  connect() {
    if (this.state === 'open' || this.state === 'connecting') {
      return Promise.resolve();
    }

    this.setState('connecting');
    const WebSocketCtor = this.options.WebSocketCtor ?? globalThis.WebSocket;
    const socketURL = this.options.token.trim()
      ? desktopWsUrlWithToken(this.options.url, this.options.token)
      : this.options.url;

    return new Promise<void>((resolve, reject) => {
      let settled = false;
      let connectTimer: ReturnType<typeof setTimeout> | undefined;
      try {
        const socket = new WebSocketCtor(socketURL);
        this.socket = socket;

        const finish = () => {
          if (settled) {
            return;
          }
          settled = true;
          if (connectTimer) {
            clearTimeout(connectTimer);
          }
          this.setState('open');
          resolve();
        };
        const fail = (message = 'WebSocket connection failed') => {
          if (settled) {
            return;
          }
          settled = true;
          if (connectTimer) {
            clearTimeout(connectTimer);
          }
          this.setState('error');
          reject(new Error(message));
        };

        connectTimer = setTimeout(() => {
          socket.close(1000, 'connect timeout');
          fail('WebSocket connection timed out');
        }, this.options.connectTimeoutMs ?? DEFAULT_CONNECT_TIMEOUT_MS);

        socket.addEventListener('open', finish);
        socket.addEventListener('error', () => fail());
        socket.addEventListener('message', (event) => this.handleMessage((event as MessageEvent).data));
        socket.addEventListener('close', () => {
          this.rejectPending(new Error('WebSocket closed'));
          this.setState(this.state === 'error' ? 'error' : 'closed');
        });
      } catch (error) {
        if (connectTimer) {
          clearTimeout(connectTimer);
        }
        this.setState('error');
        reject(error instanceof Error ? error : new Error(String(error)));
      }
    });
  }

  close() {
    this.socket?.close(1000, 'client closing');
    this.socket = null;
    this.rejectPending(new Error('WebSocket closed'));
    this.setState('closed');
  }

  onMessage(listener: (frame: DesktopFrame) => void) {
    this.messageListeners.add(listener);
    return () => this.messageListeners.delete(listener);
  }

  onPush(listener: (frame: DesktopFrame) => void) {
    this.pushListeners.add(listener);
    return () => this.pushListeners.delete(listener);
  }

  onState(listener: (state: ConnectionState) => void) {
    this.stateListeners.add(listener);
    return () => this.stateListeners.delete(listener);
  }

  request(ns: DesktopNamespace, type: string, payload: unknown = {}, timeoutMs = this.options.requestTimeoutMs ?? DEFAULT_REQUEST_TIMEOUT_MS) {
    const frame = createDesktopRequest(ns, type, payload);
    return this.sendRequest(frame, timeoutMs);
  }

  sendRequest(frame: DesktopRequest, timeoutMs = this.options.requestTimeoutMs ?? DEFAULT_REQUEST_TIMEOUT_MS) {
    if (!this.socket || this.state !== 'open') {
      return Promise.reject(new Error('WebSocket is not connected'));
    }

    return new Promise<DesktopResponse>((resolve, reject) => {
      const timer = setTimeout(() => {
        this.pending.delete(frame.id);
        reject(new Error(`${frame.type} timed out`));
      }, timeoutMs);
      this.pending.set(frame.id, { resolve, reject, timer });
      this.socket?.send(JSON.stringify(frame));
    });
  }

  private handleMessage(raw: unknown) {
    let frame: DesktopFrame;
    try {
      frame = parseDesktopFrame(raw);
    } catch {
      return;
    }

    for (const listener of this.messageListeners) {
      listener(frame);
    }

    if (frame.frame === 'push') {
      for (const listener of this.pushListeners) {
        listener(frame);
      }
      return;
    }

    const id = readString(frame.id);
    if (!id) {
      return;
    }
    const pending = this.pending.get(id);
    if (!pending) {
      return;
    }
    clearTimeout(pending.timer);
    this.pending.delete(id);
    if (frame.frame === 'error') {
      pending.reject(new Error(frame.msg || `${frame.type || 'request'} failed`));
      return;
    }
    pending.resolve(frame as DesktopResponse);
  }

  private rejectPending(error: Error) {
    for (const pending of this.pending.values()) {
      clearTimeout(pending.timer);
      pending.reject(error);
    }
    this.pending.clear();
  }

  private setState(nextState: ConnectionState) {
    this.state = nextState;
    for (const listener of this.stateListeners) {
      listener(nextState);
    }
  }
}

function desktopWsUrlWithToken(rawURL: string, token: string) {
  const url = new URL(rawURL);
  url.searchParams.set('token', token.trim());
  return url.toString();
}

function normalizeTaskBoardIssue(value: unknown): TaskBoardIssue | null {
  const record = asRecord(value);
  const id = readString(record.id) || readString(record.issueId);
  const title = readString(record.title);
  if (!id || !title) {
    return null;
  }
  const status = normalizeStatus(record.status);
  const priority = normalizePriority(record.priority);
  return {
    ...record,
    id,
    title,
    description: readString(record.description),
    status,
    priority,
    assigneeAgentKey: readString(record.assigneeAgentKey),
    assigneeAgentName: readString(record.assigneeAgentName),
    runState: readString(record.runState),
    updatedAt: readString(record.updatedAt),
    createdAt: readString(record.createdAt)
  };
}

function normalizeStatus(value: unknown): TaskBoardStatus {
  const status = readString(value);
  if (status === 'todo' || status === 'in_progress' || status === 'in_review' || status === 'completed') {
    return status;
  }
  return 'backlog';
}

function normalizePriority(value: unknown): TaskBoardPriority {
  const priority = readString(value);
  if (priority === 'low' || priority === 'high') {
    return priority;
  }
  return 'medium';
}

function asRecord(value: unknown): Record<string, unknown> {
  return value && typeof value === 'object' && !Array.isArray(value) ? value as Record<string, unknown> : {};
}

function readString(value: unknown) {
  return typeof value === 'string' ? value.trim() : '';
}

function readNumber(value: unknown) {
  const parsed = Number(value);
  return Number.isFinite(parsed) ? parsed : undefined;
}
