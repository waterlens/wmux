import { AlertTriangle, RefreshCw } from 'lucide-react';
import { Component, type ReactNode } from 'react';
import { Button } from './UI';

type State = { error: Error | null };

/** Rendering errors are intentionally contained; server logs never receive terminal content. */
export class ErrorBoundary extends Component<{ children: ReactNode }, State> {
  state: State = { error: null };

  static getDerivedStateFromError(error: Error): State {
    return { error };
  }

  render() {
    if (!this.state.error) return this.props.children;
    return (
      <main className="fatal-screen">
        <span className="fatal-screen__icon">
          <AlertTriangle size={24} />
        </span>
        <h1>页面需要重新载入</h1>
        <p>可能有新版本已经部署，或终端界面未能完整载入。后台会话不会因此结束。</p>
        <Button tone="primary" onClick={() => window.location.reload()}>
          <RefreshCw size={16} /> 重新载入
        </Button>
      </main>
    );
  }
}
