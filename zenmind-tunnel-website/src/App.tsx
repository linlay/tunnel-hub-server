import { FormEvent, ReactNode, useCallback, useEffect, useMemo, useState } from 'react';
import {
  Activity,
  AlertTriangle,
  CheckCircle2,
  Copy,
  KeyRound,
  LogOut,
  Pencil,
  Plus,
  RefreshCcw,
  Route as RouteIcon,
  Save,
  Server,
  ShieldCheck,
  Trash2,
  Wifi
} from 'lucide-react';
import {
  AgentSession,
  ApiError,
  EventLog,
  Metrics,
  Route,
  TunnelToken,
  api
} from './lib/api';

type LoadState = 'loading' | 'ready' | 'anonymous';

type RouteForm = {
  id?: string;
  publicHost: string;
  targetUrl: string;
  active: boolean;
};

const emptyRoute: RouteForm = {
  publicHost: '',
  targetUrl: 'http://127.0.0.1:3000',
  active: true
};

export function App() {
  const [loadState, setLoadState] = useState<LoadState>('loading');
  const [username, setUsername] = useState('');

  useEffect(() => {
    api
      .me()
      .then((me) => {
        setUsername(me.username);
        setLoadState('ready');
      })
      .catch(() => setLoadState('anonymous'));
  }, []);

  if (loadState === 'loading') {
    return <div className="boot">Zenmind Tunnel</div>;
  }

  if (loadState === 'anonymous') {
    return (
      <Login
        onLogin={(name) => {
          setUsername(name);
          setLoadState('ready');
        }}
      />
    );
  }

  return (
    <Dashboard
      username={username}
      onLogout={() => {
        setUsername('');
        setLoadState('anonymous');
      }}
    />
  );
}

function Login({ onLogin }: { onLogin: (username: string) => void }) {
  const [username, setUsername] = useState('admin');
  const [password, setPassword] = useState('admin');
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);

  async function submit(event: FormEvent) {
    event.preventDefault();
    setBusy(true);
    setError('');
    try {
      const response = await api.login(username, password);
      onLogin(response.username);
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  }

  return (
    <main className="login-shell">
      <form className="login-panel" onSubmit={submit}>
        <div className="login-mark">
          <ShieldCheck size={24} />
        </div>
        <h1>Zenmind Tunnel</h1>
        <label>
          Username
          <input value={username} onChange={(event) => setUsername(event.target.value)} />
        </label>
        <label>
          Password
          <input
            type="password"
            value={password}
            onChange={(event) => setPassword(event.target.value)}
          />
        </label>
        {error ? <p className="error-line">{error}</p> : null}
        <button className="primary wide" type="submit" disabled={busy}>
          <ShieldCheck size={16} />
          {busy ? 'Signing in' : 'Sign in'}
        </button>
      </form>
    </main>
  );
}

function Dashboard({ username, onLogout }: { username: string; onLogout: () => void }) {
  const [routes, setRoutes] = useState<Route[]>([]);
  const [tokens, setTokens] = useState<TunnelToken[]>([]);
  const [sessions, setSessions] = useState<AgentSession[]>([]);
  const [events, setEvents] = useState<EventLog[]>([]);
  const [metrics, setMetrics] = useState<Metrics>({
    hasActiveAgent: false,
    totalStreams: 0,
    activeStreams: 0
  });
  const [routeForm, setRouteForm] = useState<RouteForm>(emptyRoute);
  const [tokenName, setTokenName] = useState('primary-agent');
  const [newSecret, setNewSecret] = useState('');
  const [notice, setNotice] = useState('');
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);

  const refresh = useCallback(async () => {
    setError('');
    try {
      const [nextRoutes, nextTokens, nextSessions, nextEvents, nextMetrics] = await Promise.all([
        api.routes(),
        api.tokens(),
        api.sessions(),
        api.events(),
        api.metrics()
      ]);
      setRoutes(nextRoutes ?? []);
      setTokens(nextTokens ?? []);
      setSessions(nextSessions ?? []);
      setEvents(nextEvents ?? []);
      setMetrics(nextMetrics);
    } catch (err) {
      setError(errorMessage(err));
    }
  }, []);

  useEffect(() => {
    refresh();
    const timer = window.setInterval(refresh, 5000);
    return () => window.clearInterval(timer);
  }, [refresh]);

  const activeRoutes = useMemo(() => routes.filter((route) => route.active).length, [routes]);

  async function saveRoute(event: FormEvent) {
    event.preventDefault();
    setBusy(true);
    setError('');
    setNotice('');
    try {
      if (routeForm.id) {
        await api.updateRoute(routeForm.id, routeForm);
        setNotice('Route saved');
      } else {
        await api.createRoute(routeForm);
        setNotice('Route created');
      }
      setRouteForm(emptyRoute);
      await refresh();
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  }

  async function removeRoute(route: Route) {
    setBusy(true);
    setError('');
    setNotice('');
    try {
      await api.deleteRoute(route.id);
      setNotice('Route deleted');
      await refresh();
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  }

  async function createToken(event: FormEvent) {
    event.preventDefault();
    setBusy(true);
    setError('');
    setNotice('');
    try {
      const created = await api.createToken(tokenName);
      setNewSecret(created.secret);
      setNotice('Token created');
      await refresh();
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  }

  async function removeToken(token: TunnelToken) {
    setBusy(true);
    setError('');
    setNotice('');
    try {
      await api.deleteToken(token.id);
      setNotice('Token deactivated');
      await refresh();
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  }

  async function logout() {
    await api.logout();
    onLogout();
  }

  return (
    <main className="app-shell">
      <aside className="sidebar">
        <div className="brand">
          <div className="brand-icon">
            <Wifi size={20} />
          </div>
          <div>
            <strong>Zenmind</strong>
            <span>Tunnel</span>
          </div>
        </div>
        <nav>
          <a href="#routes">
            <RouteIcon size={16} />
            Routes
          </a>
          <a href="#tokens">
            <KeyRound size={16} />
            Tokens
          </a>
          <a href="#sessions">
            <Server size={16} />
            Sessions
          </a>
          <a href="#events">
            <Activity size={16} />
            Events
          </a>
        </nav>
        <button className="ghost sidebar-logout" onClick={logout}>
          <LogOut size={16} />
          Sign out
        </button>
      </aside>

      <section className="workspace">
        <header className="topbar">
          <div>
            <h1>Operations</h1>
            <span>{username}</span>
          </div>
          <button className="ghost" onClick={refresh} title="Refresh">
            <RefreshCcw size={16} />
            Refresh
          </button>
        </header>

        <section className="status-grid" aria-label="Tunnel status">
          <MetricTile
            icon={metrics.hasActiveAgent ? <CheckCircle2 size={18} /> : <AlertTriangle size={18} />}
            label="Agent"
            value={metrics.hasActiveAgent ? 'Online' : 'Offline'}
            tone={metrics.hasActiveAgent ? 'good' : 'warn'}
          />
          <MetricTile icon={<RouteIcon size={18} />} label="Active routes" value={activeRoutes} />
          <MetricTile icon={<Activity size={18} />} label="Active streams" value={metrics.activeStreams} />
          <MetricTile icon={<Server size={18} />} label="Total streams" value={metrics.totalStreams} />
        </section>

        {error ? <div className="alert error">{error}</div> : null}
        {notice ? <div className="alert success">{notice}</div> : null}

        <section className="grid-two">
          <section className="panel" id="routes">
            <PanelTitle icon={<RouteIcon size={18} />} title="Routes" />
            <form className="form-grid" onSubmit={saveRoute}>
              <label>
                Public host
                <input
                  value={routeForm.publicHost}
                  onChange={(event) =>
                    setRouteForm((current) => ({ ...current, publicHost: event.target.value }))
                  }
                  placeholder="app.example.com"
                />
              </label>
              <label>
                Local target
                <input
                  value={routeForm.targetUrl}
                  onChange={(event) =>
                    setRouteForm((current) => ({ ...current, targetUrl: event.target.value }))
                  }
                  placeholder="http://127.0.0.1:3000"
                />
              </label>
              <label className="toggle-row">
                <input
                  type="checkbox"
                  checked={routeForm.active}
                  onChange={(event) =>
                    setRouteForm((current) => ({ ...current, active: event.target.checked }))
                  }
                />
                Active
              </label>
              <button className="primary" disabled={busy}>
                {routeForm.id ? <Save size={16} /> : <Plus size={16} />}
                {routeForm.id ? 'Save' : 'Add'}
              </button>
            </form>
            <DataTable
              empty="No routes"
              columns={['Host', 'Target', 'State', '']}
              rows={routes.map((route) => [
                <strong>{route.publicHost}</strong>,
                <code>{route.targetUrl}</code>,
                <StatusPill active={route.active} />,
                <div className="row-actions">
                  <button
                    className="icon"
                    title="Edit route"
                    onClick={() =>
                      setRouteForm({
                        id: route.id,
                        publicHost: route.publicHost,
                        targetUrl: route.targetUrl,
                        active: route.active
                      })
                    }
                  >
                    <Pencil size={15} />
                  </button>
                  <button className="icon danger" title="Delete route" onClick={() => removeRoute(route)}>
                    <Trash2 size={15} />
                  </button>
                </div>
              ])}
            />
          </section>

          <section className="panel" id="tokens">
            <PanelTitle icon={<KeyRound size={18} />} title="Tokens" />
            <form className="inline-form" onSubmit={createToken}>
              <input value={tokenName} onChange={(event) => setTokenName(event.target.value)} />
              <button className="primary" disabled={busy}>
                <Plus size={16} />
                Create
              </button>
            </form>
            {newSecret ? (
              <div className="secret-box">
                <code>{newSecret}</code>
                <button
                  className="icon"
                  title="Copy token"
                  onClick={() => navigator.clipboard.writeText(newSecret)}
                >
                  <Copy size={15} />
                </button>
              </div>
            ) : null}
            <DataTable
              empty="No tokens"
              columns={['Name', 'Prefix', 'Used', '']}
              rows={tokens.map((token) => [
                <strong className={token.active ? '' : 'muted'}>{token.name}</strong>,
                <code>{token.tokenPrefix}</code>,
                token.lastUsedAt ? formatTime(token.lastUsedAt) : <span className="muted">Never</span>,
                <div className="row-actions">
                  <StatusPill active={token.active} />
                  {token.active ? (
                    <button className="icon danger" title="Deactivate token" onClick={() => removeToken(token)}>
                      <Trash2 size={15} />
                    </button>
                  ) : null}
                </div>
              ])}
            />
          </section>
        </section>

        <section className="grid-two lower">
          <section className="panel" id="sessions">
            <PanelTitle icon={<Server size={18} />} title="Sessions" />
            <DataTable
              empty="No sessions"
              columns={['Session', 'Remote', 'Connected', 'State']}
              rows={sessions.map((session) => [
                <code>{shortID(session.id)}</code>,
                session.remoteAddr,
                formatTime(session.connectedAt),
                session.disconnectedAt ? <span className="muted">Closed</span> : <StatusPill active />
              ])}
            />
          </section>

          <section className="panel" id="events">
            <PanelTitle icon={<Activity size={18} />} title="Events" />
            <div className="event-list">
              {events.length === 0 ? <span className="empty">No events</span> : null}
              {events.map((event) => (
                <div className="event-row" key={event.id}>
                  <span>{event.type}</span>
                  <strong>{event.message}</strong>
                  <time>{formatTime(event.createdAt)}</time>
                </div>
              ))}
            </div>
          </section>
        </section>
      </section>
    </main>
  );
}

function MetricTile({
  icon,
  label,
  value,
  tone
}: {
  icon: ReactNode;
  label: string;
  value: string | number;
  tone?: 'good' | 'warn';
}) {
  return (
    <div className={`metric ${tone ?? ''}`}>
      <div>{icon}</div>
      <span>{label}</span>
      <strong>{value}</strong>
    </div>
  );
}

function PanelTitle({ icon, title }: { icon: ReactNode; title: string }) {
  return (
    <div className="panel-title">
      {icon}
      <h2>{title}</h2>
    </div>
  );
}

function DataTable({
  columns,
  rows,
  empty
}: {
  columns: string[];
  rows: ReactNode[][];
  empty: string;
}) {
  if (rows.length === 0) {
    return <div className="empty">{empty}</div>;
  }
  return (
    <div className="table-wrap">
      <table>
        <thead>
          <tr>
            {columns.map((column) => (
              <th key={column}>{column}</th>
            ))}
          </tr>
        </thead>
        <tbody>
          {rows.map((row, index) => (
            <tr key={index}>
              {row.map((cell, cellIndex) => (
                <td key={cellIndex}>{cell}</td>
              ))}
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

function StatusPill({ active }: { active: boolean }) {
  return <span className={`pill ${active ? 'active' : 'off'}`}>{active ? 'Active' : 'Off'}</span>;
}

function formatTime(value: string) {
  return new Intl.DateTimeFormat(undefined, {
    month: 'short',
    day: '2-digit',
    hour: '2-digit',
    minute: '2-digit'
  }).format(new Date(value));
}

function shortID(value: string) {
  return value.length > 14 ? `${value.slice(0, 14)}...` : value;
}

function errorMessage(err: unknown) {
  if (err instanceof ApiError) {
    return err.message;
  }
  if (err instanceof Error) {
    return err.message;
  }
  return 'Request failed';
}
