import { FormEvent, ReactNode, KeyboardEvent, useCallback, useEffect, useMemo, useRef, useState } from 'react';
import {
  Activity,
  ArrowLeft,
  ArrowRight,
  Bot,
  CheckCircle2,
  Circle,
  ClipboardList,
  KeyRound,
  LayoutDashboard,
  ListChecks,
  Loader2,
  MessageCircle,
  MessageSquareText,
  Plus,
  RefreshCcw,
  Search,
  Send,
  ShieldCheck,
  Sparkles,
  Trash2,
  Unplug,
  Wifi,
  X,
  XCircle
} from 'lucide-react';
import {
  AgentSummary,
  ConnectionState,
  DesktopFrame,
  DesktopStreamFrame,
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

type View = 'copilot' | 'board';
type LogDirection = 'in' | 'out' | 'system';
type ChatRole = 'user' | 'assistant' | 'system';
type BoardPriorityFilter = TaskBoardPriority | 'all';

type ChatMessage = {
  id: string;
  role: ChatRole;
  content: string;
  at: string;
  agentKey?: string;
  status?: 'pending' | 'done' | 'error';
  reasoning?: string;
  reasoningLabel?: string;
};

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

type RefreshAgentsOptions = {
  quiet?: boolean;
};

const visibleStatuses: TaskBoardStatus[] = ['backlog', 'todo', 'in_progress', 'in_review', 'completed'];

const statusLabel: Record<TaskBoardStatus, string> = {
  backlog: '需求池',
  todo: '待办',
  in_progress: '进行中',
  in_review: '审查中',
  completed: '已完成'
};

const statusCaption: Record<TaskBoardStatus, string> = {
  backlog: 'Ideas',
  todo: 'Ready',
  in_progress: 'Running',
  in_review: 'Review',
  completed: 'Done'
};

const priorityLabel: Record<TaskBoardPriority, string> = {
  low: '低',
  medium: '中',
  high: '高'
};

const emptySnapshot: TaskBoardSnapshot = { issues: [] };

export function App() {
  const initialToken = useMemo(() => consumeInitialToken(), []);
  const [token, setToken] = useState(initialToken);
  const [connectionState, setConnectionState] = useState<ConnectionState>('idle');
  const [activeView, setActiveView] = useState<View>('copilot');
  const [authModalOpen, setAuthModalOpen] = useState(!initialToken);
  const [diagnosticsOpen, setDiagnosticsOpen] = useState(false);
  const [snapshot, setSnapshot] = useState<TaskBoardSnapshot>(emptySnapshot);
  const [desktopAgents, setDesktopAgents] = useState<AgentSummary[]>([]);
  const [platformAgents, setPlatformAgents] = useState<AgentSummary[]>([]);
  const [logs, setLogs] = useState<LogEntry[]>([]);
  const [feedback, setFeedback] = useState<Feedback | null>(null);
  const [issueTitle, setIssueTitle] = useState('');
  const [issueStatus, setIssueStatus] = useState<TaskBoardStatus>('backlog');
  const [issuePriority, setIssuePriority] = useState<TaskBoardPriority>('medium');
  const [boardQuery, setBoardQuery] = useState('');
  const [boardPriorityFilter, setBoardPriorityFilter] = useState<BoardPriorityFilter>('all');
  const [mobileStatus, setMobileStatus] = useState<TaskBoardStatus>('todo');
  const [agentQuery, setAgentQuery] = useState('总结当前看板状态，并给我三个下一步建议');
  const [selectedAgentKey, setSelectedAgentKey] = useState('');
  const [messages, setMessages] = useState<ChatMessage[]>([]);
  const [busyAction, setBusyAction] = useState('');
  const sessionRef = useRef<DesktopWsSession | null>(null);
  const tokenRef = useRef(initialToken);
  const autoConnectStartedRef = useRef(false);

  const wsURL = useMemo(() => desktopWsUrlFromLocation(window.location), []);
  const isConnected = connectionState === 'open';
  const allAgents = useMemo(() => mergeAgents(desktopAgents, platformAgents), [desktopAgents, platformAgents]);
  const selectedAgent = selectedAgentKey || allAgents[0]?.agentKey || 'zenmi';
  const selectedAgentSummary = allAgents.find((agent) => agent.agentKey === selectedAgent);
  const filteredIssues = useMemo(
    () => filterIssues(snapshot.issues, boardQuery, boardPriorityFilter),
    [boardPriorityFilter, boardQuery, snapshot.issues]
  );
  const groupedIssues = useMemo(() => groupIssues(filteredIssues), [filteredIssues]);
  const boardSummary = useMemo(() => summarizeBoard(snapshot.issues), [snapshot.issues]);

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
        at: formatClock(),
        direction,
        title,
        detail: detail === undefined
          ? undefined
          : redactSensitiveText(typeof detail === 'string' ? detail : JSON.stringify(detail, null, 2), [tokenRef.current])
      },
      ...current
    ].slice(0, 120));
  }, []);

  const addMessage = useCallback((message: Omit<ChatMessage, 'id' | 'at'>) => {
    const nextMessage: ChatMessage = {
      id: createLocalId('msg'),
      at: formatClock(),
      ...message
    };
    setMessages((current) => [...current, nextMessage]);
    return nextMessage.id;
  }, []);

  const updateMessage = useCallback((id: string, patch: Partial<ChatMessage>) => {
    setMessages((current) => current.map((message) => message.id === id ? { ...message, ...patch } : message));
  }, []);

  const updateMessageWith = useCallback((id: string, updater: (message: ChatMessage) => ChatMessage) => {
    setMessages((current) => current.map((message) => message.id === id ? updater(message) : message));
  }, []);

  const request = useCallback(async (
    ns: 'd' | 'ap',
    type: string,
    payload: unknown = {},
    options: {
      silent?: boolean;
      timeoutMs?: number;
      onStream?: (frame: DesktopStreamFrame) => void;
      resolveOnStreamDone?: boolean;
    } = {}
  ) => {
    const session = sessionRef.current;
    if (!session || session.readyState !== 'open') {
      throw new Error('WebSocket is not connected');
    }
    if (!options.silent) {
      addLog('out', `${ns}:${type}`, payload);
    }
    const response = await session.request(ns, type, payload, {
      timeoutMs: options.timeoutMs,
      onStream: options.onStream,
      resolveOnStreamDone: options.resolveOnStreamDone
    });
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
      setFeedback({ tone: 'success', message: '看板已刷新' });
    } catch (error) {
      showError(setFeedback, error);
    } finally {
      setBusyAction('');
    }
  }, [request]);

  const refreshAgents = useCallback(async (options: RefreshAgentsOptions = {}) => {
    setBusyAction('refresh-agents');
    try {
      const [desktop, platform] = await Promise.allSettled([
        request('d', 'agent.list', {}, { silent: true, timeoutMs: 3_500 }),
        request('ap', '/api/agents', { includeChats: 3 }, { silent: false, timeoutMs: 16_000 })
      ]);
      if (desktop.status === 'fulfilled') {
        setDesktopAgents(normalizeAgents(desktop.value, 'desktop'));
      } else {
        addLog('system', 'Desktop agent.list skipped', desktop.reason instanceof Error ? desktop.reason.message : String(desktop.reason));
      }
      if (platform.status === 'fulfilled') {
        setPlatformAgents(normalizeAgents(platform.value, 'agent-platform'));
      } else {
        addLog('system', 'Agent platform list failed', platform.reason instanceof Error ? platform.reason.message : String(platform.reason));
      }
      if (desktop.status === 'rejected' && platform.status === 'rejected') {
        setPlatformAgents((current) => current.length ? current : [createFallbackAgent()]);
        if (!options.quiet) {
          setFeedback({ tone: 'info', message: '暂时使用默认智能体 zenmi' });
        }
        return;
      }
      if (!options.quiet) {
        setFeedback({ tone: 'success', message: '智能体已刷新' });
      }
    } catch (error) {
      setPlatformAgents((current) => current.length ? current : [createFallbackAgent()]);
      addLog('system', 'Agent refresh failed', error instanceof Error ? error.message : String(error));
      if (!options.quiet) {
        setFeedback({ tone: 'info', message: '暂时使用默认智能体 zenmi' });
      }
    } finally {
      setBusyAction('');
    }
  }, [addLog, request]);

  const bootstrapSession = useCallback(async () => {
    await request('d', 'session.hello', {}, { silent: true });
    await request('d', 'event.subscribe', {
      types: ['snapshot.updated', 'issue.created', 'issue.updated', 'issue.deleted', 'issue.moved', 'agent.catalog.updated']
    }, { silent: true });
    await Promise.allSettled([refreshBoard(), refreshAgents({ quiet: true })]);
  }, [refreshAgents, refreshBoard, request]);

  const connectWithToken = useCallback(async (rawToken: string, mode: 'manual' | 'auto' = 'manual') => {
    const trimmedToken = rawToken.trim();
    if (!trimmedToken) {
      setFeedback({ tone: 'error', message: '需要 Desktop token' });
      setAuthModalOpen(true);
      return;
    }
    sessionRef.current?.close();
    const session = new DesktopWsSession({ url: wsURL, token: trimmedToken, connectTimeoutMs: 12_000 });
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
      setToken(trimmedToken);
      setAuthModalOpen(false);
      setFeedback({ tone: 'success', message: mode === 'auto' ? '已用访问链接连接 Desktop' : '已连接 Desktop' });
      addLog('system', 'Connected', wsURL);
      await bootstrapSession();
    } catch (error) {
      setAuthModalOpen(true);
      showError(setFeedback, error);
      addLog('system', 'Connection failed', error instanceof Error ? error.message : String(error));
    }
  }, [addLog, bootstrapSession, refreshAgents, refreshBoard, wsURL]);

  useEffect(() => {
    if (!initialToken || autoConnectStartedRef.current) {
      return;
    }
    autoConnectStartedRef.current = true;
    void connectWithToken(initialToken, 'auto');
  }, [connectWithToken, initialToken]);

  useEffect(() => {
    if (connectionState === 'closed' || connectionState === 'error') {
      setAuthModalOpen(true);
    }
  }, [connectionState]);

  async function handleConnect(event?: FormEvent) {
    event?.preventDefault();
    await connectWithToken(token, 'manual');
  }

  function handleDisconnect() {
    sessionRef.current?.close();
    sessionRef.current = null;
    setConnectionState('closed');
    setAuthModalOpen(true);
    addLog('system', 'Disconnected');
  }

  async function handleCreateIssue(event: FormEvent) {
    event.preventDefault();
    const title = issueTitle.trim();
    if (!title) {
      setFeedback({ tone: 'error', message: '请输入任务标题' });
      return;
    }
    setBusyAction('create-issue');
    try {
      await request('d', 'issue.create', {
        title,
        status: issueStatus,
        priority: issuePriority,
        syncToCloud: false
      });
      setIssueTitle('');
      setFeedback({ tone: 'success', message: '任务已创建' });
      await refreshBoard();
      setMobileStatus(issueStatus);
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
    await setIssueStatusOnDesktop(issue, nextStatus, `move-${issue.id}`, '任务已移动');
  }

  async function updateIssueStatus(issue: TaskBoardIssue, status: TaskBoardStatus) {
    if (status === issue.status) {
      return;
    }
    await setIssueStatusOnDesktop(issue, status, `status-${issue.id}`, '状态已更新');
  }

  async function completeIssue(issue: TaskBoardIssue) {
    await setIssueStatusOnDesktop(issue, 'completed', `complete-${issue.id}`, '任务已完成');
  }

  async function setIssueStatusOnDesktop(issue: TaskBoardIssue, status: TaskBoardStatus, actionKey: string, message: string) {
    setBusyAction(actionKey);
    try {
      if (actionKey.startsWith('move-')) {
        await request('d', 'issue.move', { issueId: issue.id, status });
      } else {
        await request('d', 'issue.update', { issueId: issue.id, input: { status } });
      }
      setSnapshot((current) => ({
        ...current,
        issues: current.issues.map((item) => item.id === issue.id ? { ...item, status } : item)
      }));
      setMobileStatus(status);
      setFeedback({ tone: 'success', message });
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
      setFeedback({ tone: 'error', message: '请输入消息' });
      return;
    }
    setActiveView('copilot');
    setBusyAction('agent-query');
    addMessage({ role: 'user', content: message, agentKey: selectedAgent, status: 'done' });
    const pendingId = addMessage({
      role: 'assistant',
      content: '正在思考...',
      agentKey: selectedAgent,
      status: 'pending'
    });
    setAgentQuery('');
    try {
      const data = await request('ap', '/api/query', {
        agentKey: selectedAgent,
        message,
        stream: true,
        includeUsage: true,
        context: {
          board: boardSummary
        }
      }, {
        timeoutMs: 120_000,
        resolveOnStreamDone: true,
        onStream: (frame) => {
          applyAgentStreamFrame(frame, pendingId, updateMessageWith);
        }
      });
      const answer = readAgentAnswer(data);
      updateMessageWith(pendingId, (current) => {
        const nextContent = answer || (current.content === '正在思考...' ? '' : current.content);
        return {
          ...current,
          content: nextContent || '已完成，但没有返回文本。',
          status: 'done'
        };
      });
      setFeedback({ tone: 'success', message: '智能体已回复' });
    } catch (error) {
      const text = error instanceof Error ? error.message : String(error);
      updateMessage(pendingId, {
        content: text,
        status: 'error'
      });
      showError(setFeedback, error);
    } finally {
      setBusyAction('');
    }
  }

  function handleComposerKeyDown(event: KeyboardEvent<HTMLTextAreaElement>) {
    if (event.key !== 'Enter' || event.shiftKey || event.nativeEvent.isComposing) {
      return;
    }
    event.preventDefault();
    event.currentTarget.form?.requestSubmit();
  }

  function fillPrompt(prompt: string) {
    setAgentQuery(prompt);
    setActiveView('copilot');
  }

  const hostLabel = window.location.host || 'Desktop public host';

  return (
    <main className="desktop-public-app">
      <header className="app-tabs-shell">
        <nav className="primary-tabs" aria-label="主工作区">
          <TabButton active={activeView === 'copilot'} onClick={() => setActiveView('copilot')} icon={<MessageCircle size={17} />} label="对话" />
          <TabButton active={activeView === 'board'} onClick={() => setActiveView('board')} icon={<ClipboardList size={17} />} label="看板" />
        </nav>
        <button className={`diagnostics-trigger ${connectionState}`} type="button" onClick={() => setDiagnosticsOpen(true)}>
          <span className="connection-dot" aria-hidden="true" />
          诊断
        </button>
      </header>

      {feedback && !authModalOpen ? <Toast feedback={feedback} onDismiss={() => setFeedback(null)} /> : null}

      <section className="main-stage" data-view={activeView}>
        {activeView === 'copilot' ? (
          <CopilotPanel
          agentQuery={agentQuery}
          agents={allAgents}
          boardSummary={boardSummary}
          busyAction={busyAction}
          isConnected={isConnected}
          messages={messages}
          selectedAgent={selectedAgent}
          selectedAgentSummary={selectedAgentSummary}
          onClearMessages={() => setMessages([])}
          onFillPrompt={fillPrompt}
          onKeyDown={handleComposerKeyDown}
          onRefreshAgents={refreshAgents}
          onRunQuery={runAgentQuery}
          setAgentQuery={setAgentQuery}
          setSelectedAgentKey={setSelectedAgentKey}
          />
        ) : (
          <BoardPanel
          boardPriorityFilter={boardPriorityFilter}
          boardQuery={boardQuery}
          busyAction={busyAction}
          groupedIssues={groupedIssues}
          isConnected={isConnected}
          issuePriority={issuePriority}
          issueStatus={issueStatus}
          issueTitle={issueTitle}
          mobileStatus={mobileStatus}
          totalFiltered={filteredIssues.length}
          totalIssues={snapshot.issues.length}
          onComplete={completeIssue}
          onCreateIssue={handleCreateIssue}
          onMove={moveIssue}
          onRefresh={refreshBoard}
          onStatusChange={updateIssueStatus}
          setBoardPriorityFilter={setBoardPriorityFilter}
          setBoardQuery={setBoardQuery}
          setIssuePriority={setIssuePriority}
          setIssueStatus={setIssueStatus}
          setIssueTitle={setIssueTitle}
          setMobileStatus={setMobileStatus}
          />
        )}
      </section>

      {authModalOpen ? (
        <AuthModal
          connectionState={connectionState}
          feedback={feedback}
          token={token}
          onClose={isConnected ? () => setAuthModalOpen(false) : undefined}
          onConnect={handleConnect}
          setToken={setToken}
        />
      ) : null}

      {diagnosticsOpen ? (
        <DiagnosticsPanel
          connectionState={connectionState}
          hostLabel={hostLabel}
          isConnected={isConnected}
          logs={logs}
          token={token}
          wsURL={wsURL}
          onClearLogs={() => setLogs([])}
          onClose={() => setDiagnosticsOpen(false)}
          onConnect={() => void connectWithToken(token, 'manual')}
          onDisconnect={handleDisconnect}
          onOpenAuth={() => setAuthModalOpen(true)}
        />
      ) : null}
    </main>
  );
}

function ConnectionPanel({
  connectionState,
  feedback,
  isConnected,
  token,
  onConnect,
  onDisconnect,
  onDismissFeedback,
  setToken
}: {
  connectionState: ConnectionState;
  feedback: Feedback | null;
  isConnected: boolean;
  token: string;
  onConnect: (event: FormEvent) => void;
  onDisconnect: () => void;
  onDismissFeedback: () => void;
  setToken: (token: string) => void;
}) {
  return (
    <section className="connection-card" aria-label="Desktop 连接">
      <form className="connect-form" onSubmit={onConnect}>
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
            连接
          </button>
          <button type="button" className="secondary" onClick={onDisconnect} disabled={!isConnected}>
            <Unplug size={16} />
            断开
          </button>
        </div>
      </form>
      {feedback ? <FeedbackLine feedback={feedback} onDismiss={onDismissFeedback} /> : null}
    </section>
  );
}

function SidePanel({
  agents,
  boardSummary,
  busyAction,
  issueTotals,
  isConnected,
  selectedAgent,
  onRefreshAgents,
  setSelectedAgentKey
}: {
  agents: AgentSummary[];
  boardSummary: BoardSummary;
  busyAction: string;
  issueTotals: Map<TaskBoardStatus, number>;
  isConnected: boolean;
  selectedAgent: string;
  onRefreshAgents: () => void;
  setSelectedAgentKey: (key: string) => void;
}) {
  return (
    <aside className="workspace-card side-card" aria-label="智能体和看板概览">
      <section className="side-section">
        <div className="section-head compact-head">
          <div>
            <h2>智能体</h2>
            <p>{agents.length} available</p>
          </div>
          <button className="icon-button" onClick={onRefreshAgents} disabled={!isConnected || busyAction === 'refresh-agents'} aria-label="刷新智能体">
            <RefreshCcw size={17} className={busyAction === 'refresh-agents' ? 'spin' : ''} />
          </button>
        </div>
        <div className="agent-list">
          {agents.length === 0 ? <div className="empty-panel"><Bot size={18} /> No agents loaded</div> : null}
          {agents.map((agent) => (
            <button
              className={`agent-row ${selectedAgent === agent.agentKey ? 'selected' : ''}`}
              key={`${agent.source}:${agent.agentKey}`}
              onClick={() => setSelectedAgentKey(agent.agentKey)}
              type="button"
            >
              <span className="agent-avatar"><Bot size={16} /></span>
              <span>
                <strong>{agent.displayName}</strong>
                <small>{agent.role || agent.agentKey}</small>
              </span>
              {agent.unreadCount ? <em>{agent.unreadCount}</em> : null}
            </button>
          ))}
        </div>
      </section>

      <section className="side-section">
        <div className="section-head compact-head">
          <div>
            <h2>看板概览</h2>
            <p>{boardSummary.total} issues</p>
          </div>
          <LayoutDashboard size={18} />
        </div>
        <div className="summary-grid">
          <SummaryMetric label="进行中" value={boardSummary.running} tone="progress" />
          <SummaryMetric label="高优先" value={boardSummary.highPriority} tone="danger" />
          <SummaryMetric label="已完成" value={boardSummary.completed} tone="good" />
          <SummaryMetric label="未分配" value={boardSummary.unassigned} tone="neutral" />
        </div>
        <div className="status-stack">
          {visibleStatuses.map((status) => (
            <div className={`status-row is-${status}`} key={status}>
              <span>{statusLabel[status]}</span>
              <strong>{issueTotals.get(status) ?? 0}</strong>
            </div>
          ))}
        </div>
      </section>
    </aside>
  );
}

function CopilotPanel({
  agentQuery,
  agents,
  boardSummary,
  busyAction,
  isConnected,
  messages,
  selectedAgent,
  selectedAgentSummary,
  onClearMessages,
  onFillPrompt,
  onKeyDown,
  onRefreshAgents,
  onRunQuery,
  setAgentQuery,
  setSelectedAgentKey
}: {
  agentQuery: string;
  agents: AgentSummary[];
  boardSummary: BoardSummary;
  busyAction: string;
  isConnected: boolean;
  messages: ChatMessage[];
  selectedAgent: string;
  selectedAgentSummary?: AgentSummary;
  onClearMessages: () => void;
  onFillPrompt: (prompt: string) => void;
  onKeyDown: (event: KeyboardEvent<HTMLTextAreaElement>) => void;
  onRefreshAgents: () => void;
  onRunQuery: (event: FormEvent) => void;
  setAgentQuery: (value: string) => void;
  setSelectedAgentKey: (key: string) => void;
}) {
  const messagesEndRef = useRef<HTMLDivElement | null>(null);
  const promptChips = useMemo(() => [
    '总结当前看板状态，并指出最值得先处理的任务',
    '根据看板，把今天适合推进的任务排个优先级',
    '帮我把高优先级任务拆成可执行的下一步',
    '用中文给团队写一段当前进展说明'
  ], []);

  useEffect(() => {
    messagesEndRef.current?.scrollIntoView?.({ block: 'end' });
  }, [messages, busyAction]);

  return (
    <section className="workspace-card copilot-card" aria-label="智能体对话">
      <div className="copilot-header">
        <div className="copilot-tools">
          <span className="agent-inline-label"><Bot size={15} />{selectedAgentSummary?.role || 'Agent'}</span>
          <select value={selectedAgent} onChange={(event) => setSelectedAgentKey(event.target.value)} aria-label="选择智能体">
            {agents.length === 0 ? <option value="zenmi">zenmi</option> : null}
            {agents.map((agent) => (
              <option key={`${agent.source || 'default'}:${agent.agentKey}`} value={agent.agentKey}>
                {agent.displayName}
              </option>
            ))}
          </select>
          <button className="icon-button" type="button" onClick={onRefreshAgents} disabled={!isConnected || busyAction === 'refresh-agents'} aria-label="刷新智能体">
            <RefreshCcw size={17} className={busyAction === 'refresh-agents' ? 'spin' : ''} />
          </button>
          <button className="icon-button" type="button" onClick={onClearMessages} disabled={messages.length === 0} aria-label="清空对话">
            <Trash2 size={17} />
          </button>
        </div>
        <div className="summary-strip" aria-label="看板摘要">
          <SummaryChip label="任务" value={boardSummary.total} />
          <SummaryChip label="运行中" value={boardSummary.running} tone="progress" />
          <SummaryChip label="待审查" value={boardSummary.reviewing} tone="violet" />
          <SummaryChip label="已完成" value={boardSummary.completed} tone="good" />
        </div>
      </div>

      <div className="message-stream" aria-live="polite">
        {messages.length === 0 ? (
          <div className="empty-chat">
            <div className="empty-chat-mark">
              <MessageSquareText size={18} />
              Desktop Copilot
            </div>
            <div className="prompt-grid">
              {promptChips.map((prompt) => (
                <button key={prompt} type="button" onClick={() => onFillPrompt(prompt)}>
                  {prompt}
                </button>
              ))}
            </div>
          </div>
        ) : null}
        {messages.map((message) => (
          <article className={`chat-message ${message.role} ${message.status || ''}`} key={message.id}>
            <div className="message-avatar">
              {message.role === 'user' ? <Circle size={15} /> : <Bot size={15} />}
            </div>
            <div className="message-bubble">
              <header>
                <strong>{message.role === 'user' ? 'You' : message.agentKey || 'Agent'}</strong>
                <time>{message.at}</time>
              </header>
              {message.reasoning ? (
                <details className="reasoning-block" open={message.status === 'pending'}>
                  <summary>{message.reasoningLabel || '思考中'}</summary>
                  <p>{message.reasoning}</p>
                </details>
              ) : null}
              <p>{message.content}</p>
            </div>
          </article>
        ))}
        <div ref={messagesEndRef} />
      </div>

      <form className="composer" onSubmit={onRunQuery}>
        <textarea
          aria-label="Agent message"
          value={agentQuery}
          onChange={(event) => setAgentQuery(event.target.value)}
          onKeyDown={onKeyDown}
          placeholder="向智能体发送消息"
        />
        <div className="composer-footer">
          <span>{isConnected ? 'Enter 发送，Shift+Enter 换行' : '等待 Desktop 连接'}</span>
          <button className="primary" type="submit" disabled={!isConnected || busyAction === 'agent-query'}>
            {busyAction === 'agent-query' ? <Loader2 className="spin" size={16} /> : <Send size={16} />}
            发送
          </button>
        </div>
      </form>
    </section>
  );
}

function BoardPanel({
  boardPriorityFilter,
  boardQuery,
  busyAction,
  groupedIssues,
  isConnected,
  issuePriority,
  issueStatus,
  issueTitle,
  mobileStatus,
  totalFiltered,
  totalIssues,
  onComplete,
  onCreateIssue,
  onMove,
  onRefresh,
  onStatusChange,
  setBoardPriorityFilter,
  setBoardQuery,
  setIssuePriority,
  setIssueStatus,
  setIssueTitle,
  setMobileStatus
}: {
  boardPriorityFilter: BoardPriorityFilter;
  boardQuery: string;
  busyAction: string;
  groupedIssues: Map<TaskBoardStatus, TaskBoardIssue[]>;
  isConnected: boolean;
  issuePriority: TaskBoardPriority;
  issueStatus: TaskBoardStatus;
  issueTitle: string;
  mobileStatus: TaskBoardStatus;
  totalFiltered: number;
  totalIssues: number;
  onComplete: (issue: TaskBoardIssue) => void;
  onCreateIssue: (event: FormEvent) => void;
  onMove: (issue: TaskBoardIssue, direction: -1 | 1) => void;
  onRefresh: () => void;
  onStatusChange: (issue: TaskBoardIssue, status: TaskBoardStatus) => void;
  setBoardPriorityFilter: (priority: BoardPriorityFilter) => void;
  setBoardQuery: (value: string) => void;
  setIssuePriority: (priority: TaskBoardPriority) => void;
  setIssueStatus: (status: TaskBoardStatus) => void;
  setIssueTitle: (title: string) => void;
  setMobileStatus: (status: TaskBoardStatus) => void;
}) {
  return (
    <section className="workspace-card board-card" aria-label="Desktop 看板">
      <div className="section-head">
        <div>
          <h2>Desktop 看板</h2>
          <p>{totalFiltered}/{totalIssues} issues</p>
        </div>
        <button className="icon-button" onClick={onRefresh} disabled={!isConnected || busyAction === 'refresh-board'} aria-label="刷新看板">
          <RefreshCcw size={17} className={busyAction === 'refresh-board' ? 'spin' : ''} />
        </button>
      </div>

      <form className="create-issue" onSubmit={onCreateIssue}>
        <input
          aria-label="New issue title"
          value={issueTitle}
          onChange={(event) => setIssueTitle(event.target.value)}
          placeholder="新任务"
        />
        <select aria-label="New issue status" value={issueStatus} onChange={(event) => setIssueStatus(event.target.value as TaskBoardStatus)}>
          {visibleStatuses.map((status) => <option key={status} value={status}>{statusLabel[status]}</option>)}
        </select>
        <select
          aria-label="Issue priority"
          value={issuePriority}
          onChange={(event) => setIssuePriority(event.target.value as TaskBoardPriority)}
        >
          <option value="low">低</option>
          <option value="medium">中</option>
          <option value="high">高</option>
        </select>
        <button className="primary compact" type="submit" disabled={!isConnected || busyAction === 'create-issue'}>
          {busyAction === 'create-issue' ? <Loader2 className="spin" size={16} /> : <Plus size={16} />}
          新增
        </button>
      </form>

      <div className="board-toolbar">
        <label className="search-field">
          <Search size={15} />
          <input aria-label="Search issues" value={boardQuery} onChange={(event) => setBoardQuery(event.target.value)} placeholder="搜索任务" />
        </label>
        <select aria-label="Filter by priority" value={boardPriorityFilter} onChange={(event) => setBoardPriorityFilter(event.target.value as BoardPriorityFilter)}>
          <option value="all">全部优先级</option>
          <option value="high">高优先</option>
          <option value="medium">中优先</option>
          <option value="low">低优先</option>
        </select>
      </div>

      <div className="board-status-switcher" aria-label="移动端看板列">
        {visibleStatuses.map((status) => (
          <button key={status} type="button" className={mobileStatus === status ? 'active' : ''} onClick={() => setMobileStatus(status)}>
            <span>{statusLabel[status]}</span>
            <strong>{groupedIssues.get(status)?.length ?? 0}</strong>
          </button>
        ))}
      </div>

      <div className="board-columns">
        {visibleStatuses.map((status) => {
          const issues = groupedIssues.get(status) ?? [];
          return (
            <section className={`board-column is-${status} ${mobileStatus === status ? 'mobile-active' : ''}`} key={status}>
              <header>
                <span>
                  <strong>{statusLabel[status]}</strong>
                  <small>{statusCaption[status]}</small>
                </span>
                <em>{issues.length}</em>
              </header>
              <div className="issue-list">
                {issues.length === 0 ? <div className="empty-column"><Circle size={14} /> 暂无任务</div> : null}
                {issues.map((issue) => (
                  <IssueCard
                    busyAction={busyAction}
                    issue={issue}
                    key={issue.id}
                    onComplete={onComplete}
                    onMove={onMove}
                    onStatusChange={onStatusChange}
                  />
                ))}
              </div>
            </section>
          );
        })}
      </div>
    </section>
  );
}

function IssueCard({
  busyAction,
  issue,
  onComplete,
  onMove,
  onStatusChange
}: {
  busyAction: string;
  issue: TaskBoardIssue;
  onComplete: (issue: TaskBoardIssue) => void;
  onMove: (issue: TaskBoardIssue, direction: -1 | 1) => void;
  onStatusChange: (issue: TaskBoardIssue, status: TaskBoardStatus) => void;
}) {
  const isMoving = busyAction === `move-${issue.id}`;
  const isCompleting = busyAction === `complete-${issue.id}`;
  const isUpdatingStatus = busyAction === `status-${issue.id}`;
  return (
    <article className={`issue-card is-priority-${issue.priority ?? 'medium'}`}>
      <div className="issue-card-top">
        <span className="issue-origin">Desktop</span>
        <PriorityBadge priority={issue.priority ?? 'medium'} />
      </div>
      <strong className="issue-title">{issue.title}</strong>
      {issue.description ? <p>{issue.description}</p> : null}
      <div className="issue-meta">
        <span>{issue.assigneeAgentName || issue.assigneeAgentKey || '未分配'}</span>
        {issue.runState ? <span>{issue.runState}</span> : null}
      </div>
      <div className="issue-controls">
        <select
          aria-label={`更新 ${issue.title} 状态`}
          value={issue.status}
          onChange={(event) => onStatusChange(issue, event.target.value as TaskBoardStatus)}
          disabled={isUpdatingStatus}
        >
          {visibleStatuses.map((status) => <option key={status} value={status}>{statusLabel[status]}</option>)}
        </select>
        <div className="issue-actions">
          <button aria-label={`左移 ${issue.title}`} onClick={() => onMove(issue, -1)} disabled={issue.status === 'backlog' || isMoving}>
            <ArrowLeft size={15} />
          </button>
          <button aria-label={`右移 ${issue.title}`} onClick={() => onMove(issue, 1)} disabled={issue.status === 'completed' || isMoving}>
            <ArrowRight size={15} />
          </button>
          <button aria-label={`完成 ${issue.title}`} onClick={() => onComplete(issue)} disabled={issue.status === 'completed' || isCompleting}>
            <CheckCircle2 size={15} />
          </button>
        </div>
      </div>
    </article>
  );
}

function LogsPanel({ logs, onClear }: { logs: LogEntry[]; onClear: () => void }) {
  return (
    <section className="workspace-card logs-card" aria-label="诊断日志">
      <div className="section-head compact-head">
        <div>
          <h2>诊断</h2>
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

function AuthModal({
  connectionState,
  feedback,
  token,
  onClose,
  onConnect,
  setToken
}: {
  connectionState: ConnectionState;
  feedback: Feedback | null;
  token: string;
  onClose?: () => void;
  onConnect: (event: FormEvent) => void;
  setToken: (token: string) => void;
}) {
  return (
    <div className="modal-layer" role="presentation">
      <section className="auth-modal" role="dialog" aria-modal="true" aria-labelledby="auth-title">
        <header>
          <div>
            <h2 id="auth-title">连接 Desktop</h2>
            <p>输入访问 token 后即可使用对话和看板。</p>
          </div>
          {onClose ? (
            <button className="icon-button" type="button" onClick={onClose} aria-label="关闭登录">
              <X size={17} />
            </button>
          ) : null}
        </header>
        <form className="auth-form" onSubmit={onConnect}>
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
          <button className="primary" type="submit" disabled={connectionState === 'connecting'}>
            {connectionState === 'connecting' ? <Loader2 className="spin" size={16} /> : <Wifi size={16} />}
            连接
          </button>
        </form>
        {feedback && feedback.tone === 'error' ? <FeedbackLine feedback={feedback} onDismiss={() => undefined} /> : null}
      </section>
    </div>
  );
}

function DiagnosticsPanel({
  connectionState,
  hostLabel,
  isConnected,
  logs,
  token,
  wsURL,
  onClearLogs,
  onClose,
  onConnect,
  onDisconnect,
  onOpenAuth
}: {
  connectionState: ConnectionState;
  hostLabel: string;
  isConnected: boolean;
  logs: LogEntry[];
  token: string;
  wsURL: string;
  onClearLogs: () => void;
  onClose: () => void;
  onConnect: () => void;
  onDisconnect: () => void;
  onOpenAuth: () => void;
}) {
  return (
    <div className="drawer-layer" role="presentation" onMouseDown={onClose}>
      <aside className="diagnostics-drawer" role="dialog" aria-modal="true" aria-label="诊断" onMouseDown={(event) => event.stopPropagation()}>
        <header>
          <div>
            <h2>诊断</h2>
            <p>{hostLabel}</p>
          </div>
          <button className="icon-button" type="button" onClick={onClose} aria-label="关闭诊断">
            <X size={17} />
          </button>
        </header>

        <section className="diagnostic-section">
          <div className="diagnostic-row">
            <span>连接状态</span>
            <StatusPill state={connectionState} />
          </div>
          <div className="diagnostic-row">
            <span>WebSocket</span>
            <code>{redactSensitiveText(wsURL, [token])}</code>
          </div>
          <div className="diagnostic-row">
            <span>Token</span>
            <strong>{token.trim() ? '已载入' : '未提供'}</strong>
          </div>
          <div className="diagnostic-actions">
            <button className="secondary compact" type="button" onClick={onOpenAuth}>
              <KeyRound size={15} />
              Token
            </button>
            <button className="primary compact" type="button" onClick={onConnect} disabled={connectionState === 'connecting'}>
              {connectionState === 'connecting' ? <Loader2 className="spin" size={15} /> : <Wifi size={15} />}
              重连
            </button>
            <button className="secondary compact" type="button" onClick={onDisconnect} disabled={!isConnected}>
              <Unplug size={15} />
              断开
            </button>
          </div>
        </section>

        <section className="diagnostic-section log-drawer-section">
          <div className="section-head compact-head">
            <div>
              <h2>日志</h2>
              <p>{logs.length} entries</p>
            </div>
            <button className="secondary compact" onClick={onClearLogs} disabled={logs.length === 0}>
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
      </aside>
    </div>
  );
}

function Toast({ feedback, onDismiss }: { feedback: Feedback; onDismiss: () => void }) {
  const icon = feedback.tone === 'success'
    ? <CheckCircle2 size={16} />
    : feedback.tone === 'error'
      ? <XCircle size={16} />
      : <ShieldCheck size={16} />;
  return (
    <div className={`toast ${feedback.tone}`} role="status">
      {icon}
      <span>{feedback.message}</span>
      <button onClick={onDismiss} aria-label="Dismiss" type="button"><X size={15} /></button>
    </div>
  );
}

function SummaryChip({ label, value, tone = 'neutral' }: { label: string; value: number; tone?: 'neutral' | 'good' | 'progress' | 'danger' | 'violet' }) {
  return (
    <span className={`summary-chip ${tone}`}>
      <strong>{value}</strong>
      {label}
    </span>
  );
}

function SummaryMetric({ label, value, tone }: { label: string; value: number; tone: 'neutral' | 'good' | 'progress' | 'danger' | 'violet' }) {
  return (
    <div className={`summary-metric ${tone}`}>
      <strong>{value}</strong>
      <span>{label}</span>
    </div>
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
      <button onClick={onDismiss} aria-label="Dismiss" type="button"><X size={15} /></button>
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

function filterIssues(issues: TaskBoardIssue[], query: string, priority: BoardPriorityFilter) {
  const normalizedQuery = query.trim().toLocaleLowerCase();
  return issues.filter((issue) => {
    if (priority !== 'all' && (issue.priority ?? 'medium') !== priority) {
      return false;
    }
    if (!normalizedQuery) {
      return true;
    }
    return [
      issue.id,
      issue.title,
      issue.description,
      issue.assigneeAgentName,
      issue.assigneeAgentKey,
      issue.runState
    ].some((value) => String(value || '').toLocaleLowerCase().includes(normalizedQuery));
  });
}

function groupIssues(issues: TaskBoardIssue[]) {
  const grouped = new Map<TaskBoardStatus, TaskBoardIssue[]>();
  for (const status of visibleStatuses) {
    grouped.set(status, []);
  }
  for (const issue of issues) {
    grouped.get(issue.status)?.push(issue);
  }
  for (const status of visibleStatuses) {
    grouped.set(status, sortIssues(grouped.get(status) ?? []));
  }
  return grouped;
}

function sortIssues(issues: TaskBoardIssue[]) {
  return [...issues].sort((left, right) => {
    const priorityDelta = priorityWeight(right.priority) - priorityWeight(left.priority);
    if (priorityDelta !== 0) {
      return priorityDelta;
    }
    return String(right.updatedAt || right.createdAt || '').localeCompare(String(left.updatedAt || left.createdAt || ''));
  });
}

function priorityWeight(priority?: TaskBoardPriority) {
  if (priority === 'high') {
    return 3;
  }
  if (priority === 'low') {
    return 1;
  }
  return 2;
}

function buildIssueTotals(issues: TaskBoardIssue[]) {
  const totals = new Map<TaskBoardStatus, number>();
  for (const status of visibleStatuses) {
    totals.set(status, 0);
  }
  for (const issue of issues) {
    totals.set(issue.status, (totals.get(issue.status) ?? 0) + 1);
  }
  return totals;
}

type BoardSummary = {
  total: number;
  running: number;
  reviewing: number;
  completed: number;
  highPriority: number;
  unassigned: number;
};

function summarizeBoard(issues: TaskBoardIssue[]): BoardSummary {
  return {
    total: issues.length,
    running: issues.filter((issue) => issue.status === 'in_progress' || issue.runState === 'running').length,
    reviewing: issues.filter((issue) => issue.status === 'in_review').length,
    completed: issues.filter((issue) => issue.status === 'completed').length,
    highPriority: issues.filter((issue) => issue.priority === 'high').length,
    unassigned: issues.filter((issue) => !issue.assigneeAgentKey && !issue.assigneeAgentName).length
  };
}

function mergeAgents(desktop: AgentSummary[], platform: AgentSummary[]) {
  const byKey = new Map<string, AgentSummary>();
  for (const agent of [createFallbackAgent(), ...desktop, ...platform]) {
    byKey.set(agent.agentKey, { ...byKey.get(agent.agentKey), ...agent });
  }
  return [...byKey.values()].sort((left, right) => left.displayName.localeCompare(right.displayName));
}

function createFallbackAgent(): AgentSummary {
  return {
    agentKey: 'zenmi',
    displayName: 'zenmi',
    role: 'Desktop Copilot',
    source: 'agent-platform'
  };
}

function frameTitle(frame: DesktopFrame) {
  const ns = frame.ns || 'd';
  return `${ns}:${frame.type || frame.frame || 'frame'}`;
}

function showError(setFeedback: (feedback: Feedback) => void, error: unknown) {
  setFeedback({ tone: 'error', message: error instanceof Error ? error.message : String(error) });
}

function applyAgentStreamFrame(
  frame: DesktopStreamFrame,
  messageId: string,
  updateMessageWith: (id: string, updater: (message: ChatMessage) => ChatMessage) => void
) {
  const event = readObject(frame.event);
  const type = readText(event.type);
  if (!type) {
    return;
  }

  if (type === 'content.start' || type === 'content.snapshot' || type === 'content.end') {
    const text = readText(event.text) || readText(event.message) || readText(event.delta);
    if (!text) {
      return;
    }
    updateMessageWith(messageId, (message) => ({
      ...message,
      content: text,
      status: type === 'content.end' || type === 'content.snapshot' ? 'done' : message.status
    }));
    return;
  }

  if (type === 'content.delta') {
    const delta = readText(event.delta) || readText(event.text) || readText(event.message);
    if (!delta) {
      return;
    }
    updateMessageWith(messageId, (message) => ({
      ...message,
      content: `${message.content === '正在思考...' ? '' : message.content}${delta}`,
      status: 'pending'
    }));
    return;
  }

  if (type === 'reasoning.start' || type === 'reasoning.snapshot' || type === 'reasoning.end') {
    const text = readText(event.text);
    const label = readText(event.reasoningLabel);
    updateMessageWith(messageId, (message) => ({
      ...message,
      reasoning: text || message.reasoning,
      reasoningLabel: label || message.reasoningLabel || '思考中',
      status: type === 'reasoning.end' && message.content ? 'done' : message.status
    }));
    return;
  }

  if (type === 'reasoning.delta') {
    const delta = readText(event.delta) || readText(event.text);
    const label = readText(event.reasoningLabel);
    if (!delta && !label) {
      return;
    }
    updateMessageWith(messageId, (message) => ({
      ...message,
      reasoning: `${message.reasoning || ''}${delta}`,
      reasoningLabel: label || message.reasoningLabel || '思考中',
      status: 'pending'
    }));
    return;
  }

  if (type === 'run.error') {
    const text = readText(event.error) || readText(event.message) || '智能体运行失败';
    updateMessageWith(messageId, (message) => ({
      ...message,
      content: text,
      status: 'error'
    }));
    return;
  }

  if (type === 'run.complete' || type === 'run.cancel') {
    updateMessageWith(messageId, (message) => ({
      ...message,
      status: message.status === 'error' ? 'error' : 'done'
    }));
  }
}

function readAgentAnswer(value: unknown) {
  const record = readObject(value);
  const result = readObject(record.result);
  const candidates = [
    record.answer,
    record.message,
    record.content,
    record.text,
    record.assistantText,
    result.answer,
    result.message,
    result.content,
    result.text
  ];
  for (const candidate of candidates) {
    if (typeof candidate === 'string' && candidate.trim()) {
      return candidate.trim();
    }
  }
  return '';
}

function readObject(value: unknown): Record<string, unknown> {
  return value && typeof value === 'object' && !Array.isArray(value) ? value as Record<string, unknown> : {};
}

function readText(value: unknown) {
  return typeof value === 'string' ? value : '';
}

function formatClock() {
  return new Date().toLocaleTimeString([], { hour12: false });
}

function createLocalId(prefix: string) {
  return `${prefix}_${Date.now().toString(36)}_${Math.random().toString(36).slice(2, 8)}`;
}
