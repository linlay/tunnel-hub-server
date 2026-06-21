import { FormEvent, ReactNode, useCallback, useEffect, useMemo, useRef, useState } from 'react';
import {
  Activity,
  Bot,
  CheckCircle2,
  ChevronLeft,
  ChevronRight,
  Circle,
  ClipboardList,
  KeyRound,
  ListChecks,
  Loader2,
  MessageSquareText,
  PlugZap,
  Plus,
  RefreshCcw,
  Send,
  ShieldCheck,
  Unplug,
  Wifi,
  XCircle
} from 'lucide-react';
import {
  AgentSummary,
  ConnectionState,
  DesktopFrame,
  DesktopWsSession,
  TaskBoardIssue,
  TaskBoardPriority,
  TaskBoardSnapshot,
  TaskBoardStatus,
  consumeTokenFromURL,
  desktopWsUrlFromLocation,
  extractResponsePayload,
  normalizeAgents,
  normalizeTaskBoardSnapshot,
  redactSensitiveText
} from './lib/desktopWsClient';

type View = 'board' | 'agents' | 'logs';
type LogDirection = 'in' | 'out' | 'system';

type LogEntry = {
  id: number;
  at: string;
  direction: LogDirection;
  title: string;
  detail?: string;
};

type Feedback = {
  tone: 'success' | 'error' | 'info';
  message: string;
};

const visibleStatuses: TaskBoardStatus[] = ['backlog', 'todo', 'in_progress', 'in_review', 'completed'];
const statusLabel: Record<TaskBoardStatus, string> = {
  backlog: 'Backlog',
  todo: 'Todo',
  in_progress: 'Progress',
  in_review: 'Review',
  completed: 'Done'
};

const priorityLabel: Record<TaskBoardPriority, string> = {
  low: 'Low',
  medium: 'Medium',
  high: 'High'
};

const emptySnapshot: TaskBoardSnapshot = { issues: [] };

export function App() {
  const initialToken = useMemo(() => consumeInitialToken(), []);
  const [token, setToken] = useState(initialToken);
  const [connectionState, setConnectionState] = useState<ConnectionState>('idle');
  const [activeView, setActiveView] = useState<View>('board');
  const [snapshot, setSnapshot] = useState<TaskBoardSnapshot>(emptySnapshot);
  const [desktopAgents, setDesktopAgents] = useState<AgentSummary[]>([]);
  const [platformAgents, setPlatformAgents] = useState<AgentSummary[]>([]);
  const [logs, setLogs] = useState<LogEntry[]>([]);
  const [feedback, setFeedback] = useState<Feedback | null>(null);
  const [issueTitle, setIssueTitle] = useState('');
  const [issuePriority, setIssuePriority] = useState<TaskBoardPriority>('medium');
  const [agentQuery, setAgentQuery] = useState('帮我用一句话总结当前看板状态');
  const [selectedAgentKey, setSelectedAgentKey] = useState('');
  const [queryResult, setQueryResult] = useState('');
  const [busyAction, setBusyAction] = useState('');
  const sessionRef = useRef<DesktopWsSession | null>(null);
  const tokenRef = useRef(initialToken);

  const wsURL = useMemo(() => desktopWsUrlFromLocation(window.location), []);
  const isConnected = connectionState === 'open';
  const issueCountByStatus = useMemo(() => groupIssues(snapshot.issues), [snapshot.issues]);
  const allAgents = useMemo(() => mergeAgents(desktopAgents, platformAgents), [desktopAgents, platformAgents]);
  const selectedAgent = selectedAgentKey || allAgents[0]?.agentKey || 'zenmi';

  useEffect(() => {
    tokenRef.current = token;
  }, [token]);

  useEffect(() => {
    if (!selectedAgentKey && allAgents[0]?.agentKey) {
      setSelectedAgentKey(allAgents[0].agentKey);
    }
  }, [allAgents, selectedAgentKey]);

  const addLog = useCallback((direction: LogDirection, title: string, detail?: unknown) => {
    setLogs((current) => [
      {
        id: Date.now() + Math.random(),
        at: new Date().toLocaleTimeString([], { hour12: false }),
        direction,
        title,
        detail: detail === undefined
          ? undefined
          : redactSensitiveText(typeof detail === 'string' ? detail : JSON.stringify(detail, null, 2), [tokenRef.current])
      },
      ...current
    ].slice(0, 80));
  }, []);

  const request = useCallback(async (
    ns: 'd' | 'ap',
    type: string,
    payload: unknown = {},
    options: { silent?: boolean } = {}
  ) => {
    const session = sessionRef.current;
    if (!session || session.readyState !== 'open') {
      throw new Error('WebSocket is not connected');
    }
    if (!options.silent) {
      addLog('out', `${ns}:${type}`, payload);
    }
    const response = await session.request(ns, type, payload);
    if (!options.silent) {
      addLog('in', `${response.ns || ns}:${response.type || type}`, response);
    }
    return extractResponsePayload(response);
  }, [addLog]);

  const refreshBoard = useCallback(async () => {
    setBusyAction('refresh-board');
    try {
      const data = await request('d', 'snapshot.get', {}, { silent: false });
      setSnapshot(normalizeTaskBoardSnapshot(data));
      setFeedback({ tone: 'success', message: 'Board refreshed.' });
    } catch (error) {
      showError(setFeedback, error);
    } finally {
      setBusyAction('');
    }
  }, [request]);

  const refreshAgents = useCallback(async () => {
    setBusyAction('refresh-agents');
    try {
      const [desktop, platform] = await Promise.allSettled([
        request('d', 'agent.list', {}, { silent: false }),
        request('ap', '/api/agents', { includeChats: 3 }, { silent: false })
      ]);
      if (desktop.status === 'fulfilled') {
        setDesktopAgents(normalizeAgents(desktop.value, 'desktop'));
      }
      if (platform.status === 'fulfilled') {
        setPlatformAgents(normalizeAgents(platform.value, 'agent-platform'));
      }
      if (desktop.status === 'rejected' && platform.status === 'rejected') {
        throw desktop.reason;
      }
      setFeedback({ tone: 'success', message: 'Agents refreshed.' });
    } catch (error) {
      showError(setFeedback, error);
    } finally {
      setBusyAction('');
    }
  }, [request]);

  const bootstrapSession = useCallback(async () => {
    await request('d', 'session.hello', {}, { silent: true });
    await request('d', 'event.subscribe', {
      types: ['snapshot.updated', 'issue.created', 'issue.updated', 'issue.deleted', 'issue.moved', 'agent.catalog.updated']
    }, { silent: true });
    await Promise.allSettled([refreshBoard(), refreshAgents()]);
  }, [refreshAgents, refreshBoard, request]);

  async function handleConnect(event?: FormEvent) {
    event?.preventDefault();
    const trimmedToken = token.trim();
    if (!trimmedToken) {
      setFeedback({ tone: 'error', message: 'Token is required.' });
      return;
    }
    sessionRef.current?.close();
    const session = new DesktopWsSession({ url: wsURL, token: trimmedToken });
    sessionRef.current = session;
    session.onState(setConnectionState);
    session.onMessage((frame) => {
      addLog(frame.frame === 'error' ? 'system' : 'in', frameTitle(frame), frame);
    });
    session.onPush((frame) => {
      if (frame.type?.startsWith('snapshot.') || frame.type?.startsWith('issue.')) {
        void refreshBoard();
      }
      if (frame.type === 'agent.catalog.updated') {
        void refreshAgents();
      }
    });

    try {
      await session.connect();
      setFeedback({ tone: 'success', message: 'Connected.' });
      addLog('system', 'Connected', wsURL);
      await bootstrapSession();
    } catch (error) {
      showError(setFeedback, error);
      addLog('system', 'Connection failed', error instanceof Error ? error.message : String(error));
    }
  }

  function handleDisconnect() {
    sessionRef.current?.close();
    sessionRef.current = null;
    setConnectionState('closed');
    addLog('system', 'Disconnected');
  }

  async function handleCreateIssue(event: FormEvent) {
    event.preventDefault();
    const title = issueTitle.trim();
    if (!title) {
      setFeedback({ tone: 'error', message: 'Issue title is required.' });
      return;
    }
    setBusyAction('create-issue');
    try {
      await request('d', 'issue.create', {
        title,
        status: 'backlog',
        priority: issuePriority,
        syncToCloud: false
      });
      setIssueTitle('');
      setFeedback({ tone: 'success', message: 'Issue created.' });
      await refreshBoard();
    } catch (error) {
      showError(setFeedback, error);
    } finally {
      setBusyAction('');
    }
  }

  async function moveIssue(issue: TaskBoardIssue, direction: -1 | 1) {
    const currentIndex = visibleStatuses.indexOf(issue.status);
    const nextStatus = visibleStatuses[Math.max(0, Math.min(visibleStatuses.length - 1, currentIndex + direction))];
    if (!nextStatus || nextStatus === issue.status) {
      return;
    }
    setBusyAction(`move-${issue.id}`);
    try {
      await request('d', 'issue.move', { issueId: issue.id, status: nextStatus });
      setSnapshot((current) => ({
        ...current,
        issues: current.issues.map((item) => item.id === issue.id ? { ...item, status: nextStatus } : item)
      }));
      setFeedback({ tone: 'success', message: 'Issue moved.' });
    } catch (error) {
      showError(setFeedback, error);
      await refreshBoard();
    } finally {
      setBusyAction('');
    }
  }

  async function completeIssue(issue: TaskBoardIssue) {
    setBusyAction(`complete-${issue.id}`);
    try {
      await request('d', 'issue.update', { issueId: issue.id, input: { status: 'completed' } });
      setSnapshot((current) => ({
        ...current,
        issues: current.issues.map((item) => item.id === issue.id ? { ...item, status: 'completed' } : item)
      }));
      setFeedback({ tone: 'success', message: 'Issue completed.' });
    } catch (error) {
      showError(setFeedback, error);
      await refreshBoard();
    } finally {
      setBusyAction('');
    }
  }

  async function runAgentQuery(event: FormEvent) {
    event.preventDefault();
    const message = agentQuery.trim();
    if (!message) {
      setFeedback({ tone: 'error', message: 'Message is required.' });
      return;
    }
    setBusyAction('agent-query');
    setQueryResult('');
    try {
      const data = await request('ap', '/api/query', {
        agentKey: selectedAgent,
        message,
        stream: false,
        includeUsage: true
      });
      setQueryResult(readAgentAnswer(data));
      setFeedback({ tone: 'success', message: 'Agent replied.' });
    } catch (error) {
      showError(setFeedback, error);
    } finally {
      setBusyAction('');
    }
  }

  return (
    <main className="app-shell">
      <section className="topbar">
        <div className="brand">
          <div className="brand-icon"><PlugZap size={22} /></div>
          <div>
            <h1>ZenMind Desktop</h1>
            <p>{window.location.host}</p>
          </div>
        </div>
        <StatusPill state={connectionState} />
      </section>

      <section className="connect-panel" aria-label="Desktop connection">
        <form className="connect-form" onSubmit={handleConnect}>
          <label>
            <span>Desktop token</span>
            <div className="token-field">
              <KeyRound size={16} />
              <input
                aria-label="Desktop token"
                autoComplete="off"
                value={token}
                onChange={(event) => setToken(event.target.value)}
                placeholder="Paste app token"
                type="password"
              />
            </div>
          </label>
          <div className="connect-actions">
            <button className="primary" type="submit" disabled={connectionState === 'connecting'}>
              {connectionState === 'connecting' ? <Loader2 className="spin" size={16} /> : <Wifi size={16} />}
              Connect
            </button>
            <button type="button" className="secondary" onClick={handleDisconnect} disabled={!isConnected}>
              <Unplug size={16} />
              Disconnect
            </button>
          </div>
        </form>
        {feedback ? <FeedbackLine feedback={feedback} onDismiss={() => setFeedback(null)} /> : null}
      </section>

      <nav className="tabs" aria-label="Views">
        <TabButton active={activeView === 'board'} onClick={() => setActiveView('board')} icon={<ClipboardList size={16} />} label="Board" />
        <TabButton active={activeView === 'agents'} onClick={() => setActiveView('agents')} icon={<Bot size={16} />} label="Agents" />
        <TabButton active={activeView === 'logs'} onClick={() => setActiveView('logs')} icon={<Activity size={16} />} label="Logs" />
      </nav>

      {activeView === 'board' ? (
        <BoardView
          busyAction={busyAction}
          issueCountByStatus={issueCountByStatus}
          issuePriority={issuePriority}
          issueTitle={issueTitle}
          isConnected={isConnected}
          onComplete={completeIssue}
          onCreateIssue={handleCreateIssue}
          onMove={moveIssue}
          onRefresh={refreshBoard}
          setIssuePriority={setIssuePriority}
          setIssueTitle={setIssueTitle}
        />
      ) : null}

      {activeView === 'agents' ? (
        <AgentsView
          agentQuery={agentQuery}
          agents={allAgents}
          busyAction={busyAction}
          isConnected={isConnected}
          queryResult={queryResult}
          selectedAgent={selectedAgent}
          onRefresh={refreshAgents}
          onRunQuery={runAgentQuery}
          setAgentQuery={setAgentQuery}
          setSelectedAgentKey={setSelectedAgentKey}
        />
      ) : null}

      {activeView === 'logs' ? <LogsView logs={logs} onClear={() => setLogs([])} /> : null}
    </main>
  );
}

function BoardView({
  busyAction,
  issueCountByStatus,
  issuePriority,
  issueTitle,
  isConnected,
  onComplete,
  onCreateIssue,
  onMove,
  onRefresh,
  setIssuePriority,
  setIssueTitle
}: {
  busyAction: string;
  issueCountByStatus: Map<TaskBoardStatus, TaskBoardIssue[]>;
  issuePriority: TaskBoardPriority;
  issueTitle: string;
  isConnected: boolean;
  onComplete: (issue: TaskBoardIssue) => void;
  onCreateIssue: (event: FormEvent) => void;
  onMove: (issue: TaskBoardIssue, direction: -1 | 1) => void;
  onRefresh: () => void;
  setIssuePriority: (priority: TaskBoardPriority) => void;
  setIssueTitle: (title: string) => void;
}) {
  return (
    <section className="workspace board-workspace">
      <div className="section-head">
        <div>
          <h2>Task board</h2>
          <p>{totalIssues(issueCountByStatus)} issues</p>
        </div>
        <button className="icon-button" onClick={onRefresh} disabled={!isConnected || busyAction === 'refresh-board'} aria-label="Refresh board">
          <RefreshCcw size={17} className={busyAction === 'refresh-board' ? 'spin' : ''} />
        </button>
      </div>
      <form className="create-issue" onSubmit={onCreateIssue}>
        <input
          aria-label="New issue title"
          value={issueTitle}
          onChange={(event) => setIssueTitle(event.target.value)}
          placeholder="New issue"
        />
        <select
          aria-label="Issue priority"
          value={issuePriority}
          onChange={(event) => setIssuePriority(event.target.value as TaskBoardPriority)}
        >
          <option value="low">Low</option>
          <option value="medium">Medium</option>
          <option value="high">High</option>
        </select>
        <button className="primary compact" type="submit" disabled={!isConnected || busyAction === 'create-issue'}>
          {busyAction === 'create-issue' ? <Loader2 className="spin" size={16} /> : <Plus size={16} />}
          Add
        </button>
      </form>
      <div className="board-columns">
        {visibleStatuses.map((status) => {
          const issues = issueCountByStatus.get(status) ?? [];
          return (
            <section className="board-column" key={status}>
              <header>
                <span>{statusLabel[status]}</span>
                <strong>{issues.length}</strong>
              </header>
              <div className="issue-list">
                {issues.length === 0 ? <div className="empty-column"><Circle size={14} /> Empty</div> : null}
                {issues.map((issue) => (
                  <article className="issue-card" key={issue.id}>
                    <div className="issue-card-head">
                      <strong>{issue.title}</strong>
                      <PriorityBadge priority={issue.priority ?? 'medium'} />
                    </div>
                    {issue.description ? <p>{issue.description}</p> : null}
                    <div className="issue-meta">
                      <span>{issue.assigneeAgentName || issue.assigneeAgentKey || 'Unassigned'}</span>
                      {issue.runState ? <span>{issue.runState}</span> : null}
                    </div>
                    <div className="issue-actions">
                      <button aria-label={`Move ${issue.title} left`} onClick={() => onMove(issue, -1)} disabled={status === 'backlog' || busyAction === `move-${issue.id}`}>
                        <ChevronLeft size={15} />
                      </button>
                      <button aria-label={`Move ${issue.title} right`} onClick={() => onMove(issue, 1)} disabled={status === 'completed' || busyAction === `move-${issue.id}`}>
                        <ChevronRight size={15} />
                      </button>
                      <button aria-label={`Complete ${issue.title}`} onClick={() => onComplete(issue)} disabled={status === 'completed' || busyAction === `complete-${issue.id}`}>
                        <CheckCircle2 size={15} />
                      </button>
                    </div>
                  </article>
                ))}
              </div>
            </section>
          );
        })}
      </div>
    </section>
  );
}

function AgentsView({
  agentQuery,
  agents,
  busyAction,
  isConnected,
  queryResult,
  selectedAgent,
  onRefresh,
  onRunQuery,
  setAgentQuery,
  setSelectedAgentKey
}: {
  agentQuery: string;
  agents: AgentSummary[];
  busyAction: string;
  isConnected: boolean;
  queryResult: string;
  selectedAgent: string;
  onRefresh: () => void;
  onRunQuery: (event: FormEvent) => void;
  setAgentQuery: (value: string) => void;
  setSelectedAgentKey: (key: string) => void;
}) {
  return (
    <section className="workspace agents-workspace">
      <div className="section-head">
        <div>
          <h2>Agents</h2>
          <p>{agents.length} available</p>
        </div>
        <button className="icon-button" onClick={onRefresh} disabled={!isConnected || busyAction === 'refresh-agents'} aria-label="Refresh agents">
          <RefreshCcw size={17} className={busyAction === 'refresh-agents' ? 'spin' : ''} />
        </button>
      </div>
      <div className="agent-grid">
        {agents.length === 0 ? <div className="empty-panel"><Bot size={18} /> No agents loaded</div> : null}
        {agents.map((agent) => (
          <button
            className={`agent-card ${selectedAgent === agent.agentKey ? 'selected' : ''}`}
            key={`${agent.source}:${agent.agentKey}`}
            onClick={() => setSelectedAgentKey(agent.agentKey)}
          >
            <Bot size={18} />
            <span>
              <strong>{agent.displayName}</strong>
              <small>{agent.role || agent.agentKey}</small>
            </span>
            {agent.unreadCount ? <em>{agent.unreadCount}</em> : null}
          </button>
        ))}
      </div>
      <form className="agent-query" onSubmit={onRunQuery}>
        <label>
          <span>Agent</span>
          <select value={selectedAgent} onChange={(event) => setSelectedAgentKey(event.target.value)} aria-label="Agent">
            {agents.length === 0 ? <option value="zenmi">zenmi</option> : null}
            {agents.map((agent) => (
              <option key={`${agent.source}:${agent.agentKey}`} value={agent.agentKey}>
                {agent.displayName}
              </option>
            ))}
          </select>
        </label>
        <label>
          <span>Message</span>
          <textarea value={agentQuery} onChange={(event) => setAgentQuery(event.target.value)} aria-label="Agent message" />
        </label>
        <button className="primary" type="submit" disabled={!isConnected || busyAction === 'agent-query'}>
          {busyAction === 'agent-query' ? <Loader2 className="spin" size={16} /> : <Send size={16} />}
          Send
        </button>
      </form>
      {queryResult ? (
        <div className="query-result">
          <MessageSquareText size={18} />
          <p>{queryResult}</p>
        </div>
      ) : null}
    </section>
  );
}

function LogsView({ logs, onClear }: { logs: LogEntry[]; onClear: () => void }) {
  return (
    <section className="workspace logs-workspace">
      <div className="section-head">
        <div>
          <h2>Diagnostics</h2>
          <p>{logs.length} entries</p>
        </div>
        <button className="secondary compact" onClick={onClear} disabled={logs.length === 0}>
          Clear
        </button>
      </div>
      <div className="log-list">
        {logs.length === 0 ? <div className="empty-panel"><ListChecks size={18} /> No logs yet</div> : null}
        {logs.map((entry) => (
          <article className={`log-entry ${entry.direction}`} key={entry.id}>
            <header>
              <span>{entry.direction}</span>
              <strong>{entry.title}</strong>
              <time>{entry.at}</time>
            </header>
            {entry.detail ? <pre>{entry.detail}</pre> : null}
          </article>
        ))}
      </div>
    </section>
  );
}

function StatusPill({ state }: { state: ConnectionState }) {
  const label = {
    idle: 'Idle',
    connecting: 'Connecting',
    open: 'Online',
    closed: 'Closed',
    error: 'Error'
  }[state];
  const icon = state === 'open'
    ? <CheckCircle2 size={15} />
    : state === 'connecting'
      ? <Loader2 className="spin" size={15} />
      : state === 'error'
        ? <XCircle size={15} />
        : <Circle size={15} />;
  return <div className={`status-pill ${state}`}>{icon}{label}</div>;
}

function TabButton({ active, icon, label, onClick }: { active: boolean; icon: ReactNode; label: string; onClick: () => void }) {
  return (
    <button className={active ? 'active' : ''} onClick={onClick} type="button">
      {icon}
      {label}
    </button>
  );
}

function FeedbackLine({ feedback, onDismiss }: { feedback: Feedback; onDismiss: () => void }) {
  const icon = feedback.tone === 'success'
    ? <CheckCircle2 size={16} />
    : feedback.tone === 'error'
      ? <XCircle size={16} />
      : <ShieldCheck size={16} />;
  return (
    <div className={`feedback ${feedback.tone}`}>
      {icon}
      <span>{feedback.message}</span>
      <button onClick={onDismiss} aria-label="Dismiss" type="button">×</button>
    </div>
  );
}

function PriorityBadge({ priority }: { priority: TaskBoardPriority }) {
  return <span className={`priority ${priority}`}>{priorityLabel[priority]}</span>;
}

function consumeInitialToken() {
  const result = consumeTokenFromURL(window.location.href);
  if (result.token && result.cleanURL !== window.location.href) {
    window.history.replaceState(null, document.title, result.cleanURL);
  }
  return result.token;
}

function groupIssues(issues: TaskBoardIssue[]) {
  const grouped = new Map<TaskBoardStatus, TaskBoardIssue[]>();
  for (const status of visibleStatuses) {
    grouped.set(status, []);
  }
  for (const issue of issues) {
    grouped.get(issue.status)?.push(issue);
  }
  return grouped;
}

function totalIssues(grouped: Map<TaskBoardStatus, TaskBoardIssue[]>) {
  return [...grouped.values()].reduce((total, issues) => total + issues.length, 0);
}

function mergeAgents(desktop: AgentSummary[], platform: AgentSummary[]) {
  const byKey = new Map<string, AgentSummary>();
  for (const agent of [...desktop, ...platform]) {
    byKey.set(agent.agentKey, { ...byKey.get(agent.agentKey), ...agent });
  }
  return [...byKey.values()].sort((left, right) => left.displayName.localeCompare(right.displayName));
}

function frameTitle(frame: DesktopFrame) {
  const ns = frame.ns || 'd';
  return `${ns}:${frame.type || frame.frame || 'frame'}`;
}

function showError(setFeedback: (feedback: Feedback) => void, error: unknown) {
  setFeedback({ tone: 'error', message: error instanceof Error ? error.message : String(error) });
}

function readAgentAnswer(value: unknown) {
  const record = value && typeof value === 'object' && !Array.isArray(value) ? value as Record<string, unknown> : {};
  const candidates = [
    record.answer,
    record.message,
    record.content,
    record.text,
    record.assistantText,
    record.result
  ];
  for (const candidate of candidates) {
    if (typeof candidate === 'string' && candidate.trim()) {
      return candidate.trim();
    }
  }
  return JSON.stringify(value, null, 2);
}
