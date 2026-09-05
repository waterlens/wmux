import { ArrowRight, Clock3, Menu, PanelLeftOpen, Plus, Server, TerminalSquare, X } from 'lucide-react';
import { lazy, Suspense, useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { api, errorMessage } from '../api';
import { DEFAULT_PREFERENCES, isMobileLayout } from '../preferences';
import { sessionStatusLabel, sessionStatusTone } from '../sessionStatus';
import type { Host, Session, TerminalPreferences, Toast, User } from '../types';
import { HostManager } from './HostManager';
import { SessionDialog, RenameSessionDialog } from './SessionDialog';
import { SettingsDialog } from './SettingsDialog';
import { Sidebar } from './Sidebar';
import { Button, ConfirmDialog, EmptyState, ToastStack } from './UI';

const TerminalView = lazy(async () => {
  const module = await import('./TerminalView');
  return { default: module.TerminalView };
});

type WorkspaceProps = {
  initialHosts: Host[];
  initialSessions: Session[];
  user: User;
  version: string;
  commit?: string | undefined;
  onLogout: () => Promise<void>;
};

function loadPreferences(): TerminalPreferences {
  try {
    const stored = JSON.parse(localStorage.getItem('wmux.terminalPreferences') ?? '{}') as Partial<TerminalPreferences>;
    return {
      fontSize:
        typeof stored.fontSize === 'number'
          ? Math.min(22, Math.max(11, stored.fontSize))
          : DEFAULT_PREFERENCES.fontSize,
      cursorStyle: stored.cursorStyle === 'bar' || stored.cursorStyle === 'underline' ? stored.cursorStyle : 'block',
      cursorBlink: typeof stored.cursorBlink === 'boolean' ? stored.cursorBlink : DEFAULT_PREFERENCES.cursorBlink,
      scrollback: typeof stored.scrollback === 'number' ? stored.scrollback : DEFAULT_PREFERENCES.scrollback,
      theme: stored.theme === 'dark' || stored.theme === 'system' ? stored.theme : 'light',
    };
  } catch {
    return DEFAULT_PREFERENCES;
  }
}

function loadOpenSessionIds(sessions: Session[]): string[] {
  try {
    const stored = JSON.parse(localStorage.getItem('wmux.openSessions') ?? '[]') as unknown;
    if (!Array.isArray(stored)) return [];
    return stored.filter((id): id is string => typeof id === 'string' && sessions.some((session) => session.id === id));
  } catch {
    return [];
  }
}

function loadActiveId(openIds: string[]): string | null {
  const stored = localStorage.getItem('wmux.activeSession');
  return stored && openIds.includes(stored) ? stored : (openIds.at(-1) ?? null);
}

function sessionSubtitle(session: Session): string {
  if (session.kind === 'local') return `本机 · ${session.cwd || '~'}`;
  return `${session.hostName ?? 'SSH'} · ${session.cwd || '~'}`;
}

export function Workspace({ initialHosts, initialSessions, user, version, commit, onLogout }: WorkspaceProps) {
  const [initialOpenIds] = useState(() => loadOpenSessionIds(initialSessions));
  const [hosts, setHosts] = useState(initialHosts);
  const [sessions, setSessions] = useState(initialSessions);
  const [openIds, setOpenIds] = useState<string[]>(initialOpenIds);
  const [activeId, setActiveId] = useState<string | null>(() => loadActiveId(initialOpenIds));
  const [currentView, setCurrentView] = useState<'home' | 'terminal' | 'hosts'>(
    initialOpenIds.length ? 'terminal' : 'home',
  );
  const [sidebarCollapsed, setSidebarCollapsed] = useState(false);
  const [mobileSidebar, setMobileSidebar] = useState(false);
  const [newSessionOpen, setNewSessionOpen] = useState(false);
  const [newSessionHostId, setNewSessionHostId] = useState<string | undefined>();
  const [renameTarget, setRenameTarget] = useState<Session | null>(null);
  const [deleteTarget, setDeleteTarget] = useState<Session | null>(null);
  const [deleting, setDeleting] = useState(false);
  const [restartTarget, setRestartTarget] = useState<Session | null>(null);
  const [restartingIds, setRestartingIds] = useState<ReadonlySet<string>>(() => new Set());
  const restartingRef = useRef<Set<string>>(new Set());
  const [settingsOpen, setSettingsOpen] = useState(false);
  const [preferences, setPreferences] = useState<TerminalPreferences>(loadPreferences);
  const [generations, setGenerations] = useState<Record<string, number>>({});
  const [toasts, setToasts] = useState<Toast[]>([]);
  const toastIdRef = useRef(0);

  const notify = useCallback((message: string, tone: Toast['tone'] = 'info') => {
    const id = ++toastIdRef.current;
    setToasts((current) => [...current.slice(-3), { id, message, tone }]);
    window.setTimeout(() => setToasts((current) => current.filter((toast) => toast.id !== id)), 4200);
  }, []);

  const dismissToast = useCallback((id: number) => {
    setToasts((current) => current.filter((toast) => toast.id !== id));
  }, []);

  useEffect(() => {
    localStorage.setItem('wmux.openSessions', JSON.stringify(openIds));
    if (activeId) localStorage.setItem('wmux.activeSession', activeId);
    else localStorage.removeItem('wmux.activeSession');
  }, [activeId, openIds]);

  useEffect(() => {
    localStorage.setItem('wmux.terminalPreferences', JSON.stringify(preferences));
  }, [preferences]);

  useEffect(() => {
    const media = window.matchMedia('(prefers-color-scheme: dark)');
    const applyTheme = () => {
      const theme = preferences.theme === 'system' ? (media.matches ? 'dark' : 'light') : preferences.theme;
      document.documentElement.dataset.theme = theme;
      const themeColor = getComputedStyle(document.documentElement).getPropertyValue('--bg').trim();
      if (themeColor)
        document.querySelector<HTMLMetaElement>('meta[name="theme-color"]')?.setAttribute('content', themeColor);
    };
    applyTheme();
    media.addEventListener('change', applyTheme);
    return () => media.removeEventListener('change', applyTheme);
  }, [preferences.theme]);

  useEffect(() => {
    const viewport = window.visualViewport;
    if (!viewport) return undefined;
    const updateHeight = () => document.documentElement.style.setProperty('--app-height', `${viewport.height}px`);
    updateHeight();
    viewport.addEventListener('resize', updateHeight);
    viewport.addEventListener('scroll', updateHeight);
    return () => {
      viewport.removeEventListener('resize', updateHeight);
      viewport.removeEventListener('scroll', updateHeight);
      document.documentElement.style.removeProperty('--app-height');
    };
  }, []);

  useEffect(() => {
    let stopped = false;
    let requestId = 0;
    const refresh = async () => {
      if (stopped || document.visibilityState === 'hidden') return;
      const request = ++requestId;
      try {
        const [nextSessions, nextHosts] = await Promise.all([api.sessions(), api.hosts()]);
        // A slow earlier poll can still land after a newer one.
        if (stopped || request !== requestId) return;
        setSessions(nextSessions);
        setHosts(nextHosts);
        const existing = new Set(nextSessions.map((session) => session.id));
        setOpenIds((current) => {
          const next = current.filter((id) => existing.has(id));
          setActiveId((active) => (active && existing.has(active) ? active : (next.at(-1) ?? null)));
          if (!next.length) setCurrentView((view) => (view === 'terminal' ? 'home' : view));
          return next;
        });
      } catch {
        // Transient polling failures keep the last known list.
      }
    };
    const timer = window.setInterval(() => void refresh(), 5000);
    const onVisible = () => {
      if (document.visibilityState === 'visible') void refresh();
    };
    document.addEventListener('visibilitychange', onVisible);
    return () => {
      stopped = true;
      window.clearInterval(timer);
      document.removeEventListener('visibilitychange', onVisible);
    };
  }, []);

  const openSessions = useMemo(
    () =>
      openIds
        .map((id) => sessions.find((session) => session.id === id))
        .filter((session): session is Session => Boolean(session)),
    [openIds, sessions],
  );

  const openSession = useCallback((session: Session) => {
    setOpenIds((current) => (current.includes(session.id) ? current : [...current, session.id]));
    setActiveId(session.id);
    setCurrentView('terminal');
    setMobileSidebar(false);
  }, []);

  const closeTab = useCallback((id: string) => {
    setOpenIds((current) => {
      const index = current.indexOf(id);
      const next = current.filter((item) => item !== id);
      setActiveId((active) => {
        if (active !== id) return active;
        const replacement = next[Math.min(index, next.length - 1)] ?? null;
        if (!replacement) setCurrentView('home');
        return replacement;
      });
      return next;
    });
  }, []);

  const updateSession = useCallback((updated: Session) => {
    setSessions((current) => current.map((session) => (session.id === updated.id ? updated : session)));
  }, []);

  const createSession = useCallback((hostId?: string) => {
    setNewSessionHostId(hostId);
    setNewSessionOpen(true);
    setMobileSidebar(false);
  }, []);

  const handleCreated = useCallback(
    (session: Session) => {
      setSessions((current) => [...current.filter((item) => item.id !== session.id), session]);
      openSession(session);
      notify(`会话「${session.name}」已启动`, 'success');
    },
    [notify, openSession],
  );

  // The ref guards re-entrancy; the state only drives the UI.
  const restartSession = useCallback(
    async (session: Session) => {
      if (restartingRef.current.has(session.id)) return;
      restartingRef.current.add(session.id);
      setRestartingIds((current) => new Set(current).add(session.id));
      try {
        const updated = await api.restartSession(session.id);
        updateSession(updated);
        setGenerations((current) => ({ ...current, [session.id]: (current[session.id] ?? 0) + 1 }));
        openSession(updated);
        notify(`正在重启「${updated.name}」`, 'success');
      } catch (reason) {
        notify(errorMessage(reason), 'error');
      } finally {
        restartingRef.current.delete(session.id);
        setRestartingIds((current) => {
          const next = new Set(current);
          next.delete(session.id);
          return next;
        });
      }
    },
    [notify, openSession, updateSession],
  );

  const requestRestart = useCallback(
    (session: Session) => {
      if (restartingRef.current.has(session.id)) return;
      if (session.status === 'connecting' || session.status === 'running' || session.status === 'reconnecting') {
        setRestartTarget(session);
        return;
      }
      void restartSession(session);
    },
    [restartSession],
  );

  async function confirmRestart() {
    if (!restartTarget) return;
    await restartSession(restartTarget);
    setRestartTarget(null);
  }

  async function deleteSession() {
    if (!deleteTarget) return;
    setDeleting(true);
    try {
      const result = await api.deleteSession(deleteTarget.id);
      closeTab(deleteTarget.id);
      setSessions((current) => current.filter((session) => session.id !== deleteTarget.id));
      if (result?.warning) notify(result.warning, 'info');
      else notify(`已结束并删除「${deleteTarget.name}」`, 'success');
      setDeleteTarget(null);
    } catch (reason) {
      notify(errorMessage(reason), 'error');
    } finally {
      setDeleting(false);
    }
  }

  return (
    <main className={`workspace ${sidebarCollapsed ? 'is-sidebar-collapsed' : ''}`}>
      <div
        className={`sidebar-backdrop ${mobileSidebar ? 'is-visible' : ''}`}
        onClick={() => setMobileSidebar(false)}
      />
      <Sidebar
        sessions={sessions}
        hosts={hosts}
        user={user}
        activeSessionId={activeId}
        currentView={currentView}
        mobileOpen={mobileSidebar}
        onMobileClose={() => setMobileSidebar(false)}
        onSelectSession={openSession}
        onNewSession={() => createSession()}
        onHome={() => {
          setCurrentView('home');
          setMobileSidebar(false);
        }}
        onHosts={() => {
          setCurrentView('hosts');
          setMobileSidebar(false);
        }}
        onRename={setRenameTarget}
        onRestart={requestRestart}
        onDelete={setDeleteTarget}
        restartingIds={restartingIds}
        onSettings={() => {
          setSettingsOpen(true);
          setMobileSidebar(false);
        }}
        onCollapse={() => setSidebarCollapsed(true)}
      />

      <section className="workspace-main">
        <header className="tabbar">
          <div className="tabbar__leading">
            <button
              className="sidebar-open-button"
              onClick={() => {
                if (isMobileLayout()) setMobileSidebar(true);
                else setSidebarCollapsed(false);
              }}
              aria-label="打开侧栏"
            >
              {sidebarCollapsed ? <PanelLeftOpen size={18} /> : <Menu size={19} />}
            </button>
          </div>
          <div className="tabs" role="tablist" aria-label="打开的终端">
            {openSessions.map((session) => (
              <div
                key={session.id}
                className={`tab ${currentView === 'terminal' && activeId === session.id ? 'is-active' : ''}`}
              >
                <button
                  className="tab__main"
                  role="tab"
                  aria-selected={currentView === 'terminal' && activeId === session.id}
                  onClick={() => openSession(session)}
                >
                  <span className={`tab__status is-${sessionStatusTone(session.status)}`} />
                  <span>{session.name}</span>
                </button>
                <button
                  className="tab__close"
                  onClick={() => closeTab(session.id)}
                  aria-label={`关闭 ${session.name} 标签，会话继续运行`}
                  title="关闭标签，会话继续运行"
                >
                  <X size={14} />
                </button>
              </div>
            ))}
            <button className="new-tab-button" onClick={() => createSession()} title="新建会话">
              <Plus size={17} />
            </button>
          </div>
        </header>

        <div className={`terminal-area ${currentView === 'hosts' ? 'is-hidden' : ''}`}>
          {(currentView === 'home' || openSessions.length === 0) && (
            <Dashboard
              user={user}
              sessions={sessions}
              onNewSession={() => createSession()}
              onOpen={openSession}
              onHosts={() => setCurrentView('hosts')}
            />
          )}
          {openSessions.length > 0 && (
            <div className={`terminal-deck ${currentView !== 'terminal' ? 'is-hidden' : ''}`}>
              <Suspense
                fallback={
                  <div className="terminal-loader">
                    <span />
                    <p>正在载入终端引擎…</p>
                  </div>
                }
              >
                {openSessions.map((session) => (
                  <TerminalView
                    key={`${session.id}:${generations[session.id] ?? 0}`}
                    session={session}
                    active={activeId === session.id && currentView === 'terminal'}
                    preferences={preferences}
                    restarting={restartingIds.has(session.id)}
                    onRestart={requestRestart}
                    onTerminate={setDeleteTarget}
                    notify={notify}
                  />
                ))}
              </Suspense>
            </div>
          )}
        </div>

        {currentView === 'hosts' && (
          <HostManager
            hosts={hosts}
            onHostsChange={setHosts}
            onStartSession={(hostId) => createSession(hostId)}
            notify={notify}
          />
        )}
      </section>

      {newSessionOpen && (
        <SessionDialog
          open
          hosts={hosts}
          sessions={sessions}
          initialHostId={newSessionHostId}
          onClose={() => {
            setNewSessionOpen(false);
            setNewSessionHostId(undefined);
          }}
          onCreated={handleCreated}
        />
      )}
      {renameTarget && (
        <RenameSessionDialog session={renameTarget} onClose={() => setRenameTarget(null)} onSaved={updateSession} />
      )}
      <ConfirmDialog
        open={Boolean(restartTarget)}
        title={`重启「${restartTarget?.name ?? '会话'}」？`}
        description="当前进程会被结束，终端会重新启动。"
        confirmLabel="重启会话"
        busy={Boolean(restartTarget && restartingIds.has(restartTarget.id))}
        onCancel={() => setRestartTarget(null)}
        onConfirm={() => void confirmRestart()}
      />
      <ConfirmDialog
        open={Boolean(deleteTarget)}
        title={`结束「${deleteTarget?.name ?? '会话'}」？`}
        description="将结束进程并删除终端历史。"
        confirmLabel="结束会话"
        danger
        busy={deleting}
        onCancel={() => setDeleteTarget(null)}
        onConfirm={() => void deleteSession()}
      />
      {settingsOpen && (
        <SettingsDialog
          open
          user={user}
          version={version}
          commit={commit}
          hosts={hosts}
          sessions={sessions}
          preferences={preferences}
          onPreferencesChange={setPreferences}
          onClose={() => setSettingsOpen(false)}
          onLogout={onLogout}
        />
      )}
      <ToastStack className={mobileSidebar ? 'is-drawer-open' : ''} toasts={toasts} dismiss={dismissToast} />
    </main>
  );
}

type DashboardProps = {
  user: User;
  sessions: Session[];
  onNewSession: () => void;
  onOpen: (session: Session) => void;
  onHosts: () => void;
};

function Dashboard({ user, sessions, onNewSession, onOpen, onHosts }: DashboardProps) {
  const recent = [...sessions]
    .sort(
      (a, b) =>
        new Date(b.lastAttachedAt ?? b.updatedAt).getTime() - new Date(a.lastAttachedAt ?? a.updatedAt).getTime(),
    )
    .slice(0, 4);
  const running = sessions.filter((session) => session.status === 'running').length;

  return (
    <section className="dashboard">
      <header className="dashboard__hero">
        <div>
          <h1>终端会话</h1>
          <p>
            {user.username} · {running ? `${running} 个正在运行` : '当前没有运行中的会话'}
          </p>
        </div>
        <div className="dashboard__actions">
          <Button tone="primary" onClick={onNewSession}>
            <Plus size={17} /> 新建会话
          </Button>
          <Button onClick={onHosts}>
            <Server size={17} /> 管理 SSH 主机
          </Button>
        </div>
      </header>

      <section className="recent-section">
        <div className="recent-section__header">
          <div>
            <Clock3 size={17} />
            <h2>最近会话</h2>
          </div>
          {sessions.length > 4 && <span>显示最近 4 个</span>}
        </div>
        {recent.length ? (
          <div className="recent-list">
            {recent.map((session) => (
              <button key={session.id} onClick={() => onOpen(session)}>
                <span className="recent-list__icon">
                  {session.kind === 'local' ? <TerminalSquare size={18} /> : <Server size={18} />}
                </span>
                <span className="recent-list__copy">
                  <strong>{session.name}</strong>
                  <small>{sessionSubtitle(session)}</small>
                </span>
                <span className={`recent-list__status is-${sessionStatusTone(session.status)}`}>
                  {sessionStatusLabel(session.status)}
                </span>
                <ArrowRight size={17} />
              </button>
            ))}
          </div>
        ) : (
          <EmptyState
            icon={<TerminalSquare size={24} />}
            title="终端正在等待"
            description="你的第一个会话会出现在这里。"
            action={
              <Button size="sm" tone="primary" onClick={onNewSession}>
                启动本机终端
              </Button>
            }
          />
        )}
      </section>
    </section>
  );
}
