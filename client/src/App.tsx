import { AlertTriangle, Download, LoaderCircle, RefreshCw, TerminalSquare, X } from 'lucide-react';
import { useCallback, useEffect, useState } from 'react';
import { api, ApiError, AUTH_EXPIRED_EVENT, errorMessage } from './api';
import { AuthScreen } from './components/AuthScreen';
import { Button } from './components/UI';
import { Workspace } from './components/Workspace';
import { activateUpdate, PWA_UPDATE_EVENT } from './pwa';
import type { Host, Session, StatusResponse, User } from './types';

type AppState =
  | { phase: 'loading' }
  | { phase: 'setup'; status: StatusResponse }
  | { phase: 'login'; status: StatusResponse }
  | { phase: 'workspace'; status: StatusResponse; user: User; hosts: Host[]; sessions: Session[] }
  | { phase: 'error'; message: string };

export default function App() {
  const [state, setState] = useState<AppState>({ phase: 'loading' });
  const [update, setUpdate] = useState<ServiceWorkerRegistration | null>(null);

  const bootstrap = useCallback(async () => {
    try {
      const status = await api.status();
      if (status.setupRequired) {
        setState({ phase: 'setup', status });
        return;
      }
      if (!status.authenticated) {
        setState({ phase: 'login', status });
        return;
      }
      const [user, hosts, sessions] = await Promise.all([api.me(), api.hosts(), api.sessions()]);
      setState({ phase: 'workspace', status, user, hosts, sessions });
    } catch (reason) {
      if (reason instanceof ApiError && reason.status === 401) return;
      setState({ phase: 'error', message: errorMessage(reason) });
    }
  }, []);

  useEffect(() => {
    const timer = window.setTimeout(() => void bootstrap(), 0);
    return () => window.clearTimeout(timer);
  }, [bootstrap]);

  useEffect(() => {
    const authExpired = () =>
      setState((current) => ({
        phase: 'login',
        status:
          'status' in current
            ? { ...current.status, authenticated: false, setupRequired: false }
            : { authenticated: false, setupRequired: false, version: '' },
      }));
    const pwaUpdate = (event: Event) => setUpdate((event as CustomEvent<ServiceWorkerRegistration>).detail);
    window.addEventListener(AUTH_EXPIRED_EVENT, authExpired);
    window.addEventListener(PWA_UPDATE_EVENT, pwaUpdate);
    return () => {
      window.removeEventListener(AUTH_EXPIRED_EVENT, authExpired);
      window.removeEventListener(PWA_UPDATE_EVENT, pwaUpdate);
    };
  }, []);

  let content;
  if (state.phase === 'loading') content = <LoadingScreen />;
  else if (state.phase === 'error')
    content = (
      <ErrorScreen
        message={state.message}
        retry={() => {
          setState({ phase: 'loading' });
          void bootstrap();
        }}
      />
    );
  else if (state.phase === 'setup')
    content = <AuthScreen mode="setup" version={state.status.version} onAuthenticated={() => void bootstrap()} />;
  else if (state.phase === 'login')
    content = <AuthScreen mode="login" version={state.status.version} onAuthenticated={() => void bootstrap()} />;
  else
    content = (
      <Workspace
        initialHosts={state.hosts}
        initialSessions={state.sessions}
        user={state.user}
        version={state.status.version}
        commit={state.status.commit}
        onLogout={async () => {
          await api.logout();
          setState({ phase: 'login', status: { ...state.status, authenticated: false } });
        }}
      />
    );

  return (
    <>
      {content}
      {update && (
        <aside className="update-prompt" role="status">
          <span className="update-prompt__icon">
            <Download size={18} />
          </span>
          <div>
            <strong>wmux 已有新版本</strong>
            <p>重新载入即可更新，后台会话会继续运行。</p>
          </div>
          <Button tone="primary" size="sm" onClick={() => activateUpdate(update)}>
            重新载入
          </Button>
          <button className="update-prompt__close" onClick={() => setUpdate(null)} aria-label="稍后更新">
            <X size={16} />
          </button>
        </aside>
      )}
    </>
  );
}

function LoadingScreen() {
  return (
    <main className="loading-screen">
      <div className="loading-brand">
        <span className="brand__mark">
          <TerminalSquare size={23} />
        </span>
        <strong>wmux</strong>
      </div>
      <LoaderCircle className="spin" size={19} />
      <p>正在恢复工作台…</p>
    </main>
  );
}

function ErrorScreen({ message, retry }: { message: string; retry: () => void }) {
  return (
    <main className="loading-screen error-screen">
      <span className="error-screen__icon">
        <AlertTriangle size={24} />
      </span>
      <h1>无法打开 wmux</h1>
      <p>{message}</p>
      <Button tone="primary" onClick={retry}>
        <RefreshCw size={16} /> 重试连接
      </Button>
    </main>
  );
}
