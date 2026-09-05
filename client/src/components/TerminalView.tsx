import { FitAddon } from '@xterm/addon-fit';
import { Unicode11Addon } from '@xterm/addon-unicode11';
import { WebLinksAddon } from '@xterm/addon-web-links';
import { Terminal } from '@xterm/xterm';
import {
  AlertTriangle,
  ArrowDown,
  ArrowLeft,
  ArrowRight,
  ArrowUp,
  Clipboard,
  Copy,
  Eraser,
  Keyboard,
  LoaderCircle,
  Lock,
  Maximize2,
  RefreshCw,
  Server,
  Square,
  TerminalSquare,
} from 'lucide-react';
import { useCallback, useEffect, useRef, useState } from 'react';
import { api, ApiError, errorMessage } from '../api';
import { liveStatusLabel } from '../sessionStatus';
import { TerminalConnection } from '../terminalConnection';
import { resolveTerminalFontFamily, TERMINAL_SYSTEM_FONT_FAMILY } from '../terminalFonts';
import { applyTerminalModifiers, encodeCursorKey, type CursorDirection, type LiveStatus } from '../terminalProtocol';
import type { Session, TerminalPreferences } from '../types';
import { Button } from './UI';

type TerminalViewProps = {
  session: Session;
  active: boolean;
  preferences: TerminalPreferences;
  restarting?: boolean | undefined;
  onRestart: (session: Session) => void;
  onTerminate: (session: Session) => void;
  notify: (message: string, tone?: 'success' | 'error' | 'info') => void;
};

/** FitAddon measures the DOM, so a fit has to wait for the current layout to commit. */
const FIT_SETTLE_MS = 0;
/** Switching tabs reveals the panel with a transition; measure once it has landed. */
const ACTIVE_TAB_SETTLE_MS = 30;

async function attachWebglRenderer(terminal: Terminal, isCancelled: () => boolean): Promise<void> {
  try {
    const { WebglAddon } = await import('@xterm/addon-webgl');
    if (isCancelled()) return;
    const addon = new WebglAddon();
    addon.onContextLoss(() => addon.dispose());
    terminal.loadAddon(addon);
  } catch {
    // The DOM renderer stays in place.
  }
}

export function TerminalView({
  session,
  active,
  preferences,
  restarting,
  onRestart,
  onTerminate,
  notify,
}: TerminalViewProps) {
  const mountRef = useRef<HTMLDivElement>(null);
  const terminalRef = useRef<Terminal | null>(null);
  const fitRef = useRef<FitAddon | null>(null);
  const connectionRef = useRef<TerminalConnection | null>(null);
  const activeRef = useRef(active);
  const ctrlRef = useRef(false);
  const altRef = useRef(false);
  const preferencesRef = useRef(preferences);
  const [liveStatus, setLiveStatus] = useState<LiveStatus>('connecting');
  const [writer, setWriter] = useState<boolean | null>(null);
  const [ctrl, setCtrl] = useState(false);
  const [alt, setAlt] = useState(false);
  const [lastError, setLastError] = useState('');
  const [terminalReady, setTerminalReady] = useState(false);
  const [replayComplete, setReplayComplete] = useState(false);
  const [retryingBackend, setRetryingBackend] = useState(false);

  useEffect(() => {
    activeRef.current = active;
  }, [active]);

  useEffect(() => {
    preferencesRef.current = preferences;
  }, [preferences]);

  const clearModifiers = useCallback(() => {
    ctrlRef.current = false;
    altRef.current = false;
    setCtrl(false);
    setAlt(false);
  }, []);

  const fit = useCallback(() => {
    const mount = mountRef.current;
    const terminal = terminalRef.current;
    const addon = fitRef.current;
    if (!mount || !terminal || !addon || !activeRef.current || mount.clientWidth < 20 || mount.clientHeight < 20)
      return;
    try {
      addon.fit();
      if (terminal.cols > 0 && terminal.rows > 0) connectionRef.current?.resize(terminal.cols, terminal.rows);
    } catch {
      // A transient zero-sized container can make fit fail during mobile rotation.
    }
  }, []);

  /** Preference, replay and write-lock changes all resize the viewport a beat later. */
  const scheduleFit = useCallback(() => {
    window.setTimeout(fit, FIT_SETTLE_MS);
  }, [fit]);

  const sendInput = useCallback(
    (value: string) => {
      // While replay is still draining this can be a device reply from historical
      // output, so the toolbar modifiers stay armed until something is really sent.
      const sent = connectionRef.current?.send(applyTerminalModifiers(value, ctrlRef.current, altRef.current));
      if (sent) clearModifiers();
    },
    [clearModifiers],
  );

  useEffect(() => {
    const mount = mountRef.current;
    if (!mount) return undefined;

    let disposed = false;
    let inputDisposable: { dispose(): void } | undefined;
    let binaryDisposable: { dispose(): void } | undefined;
    let resizeObserver: ResizeObserver | undefined;

    const terminal = new Terminal({
      // Unicode providers are still exposed through xterm's proposed API.
      allowProposedApi: true,
      allowTransparency: true,
      convertEol: false,
      disableStdin: true,
      drawBoldTextInBrightColors: true,
      fontFamily: TERMINAL_SYSTEM_FONT_FAMILY,
      fontWeight: '400',
      fontWeightBold: '600',
      letterSpacing: 0,
      lineHeight: 1.18,
      macOptionIsMeta: true,
      minimumContrastRatio: 4.5,
      rescaleOverlappingGlyphs: true,
      rightClickSelectsWord: true,
      scrollOnUserInput: true,
    });
    const fitAddon = new FitAddon();
    terminal.loadAddon(fitAddon);
    terminal.loadAddon(new Unicode11Addon());
    terminal.unicode.activeVersion = '11';
    terminal.loadAddon(
      new WebLinksAddon((_event, uri) => {
        window.open(uri, '_blank', 'noopener,noreferrer');
      }),
    );

    // Opening xterm before the webfonts resolve would cache fallback glyph metrics.
    void resolveTerminalFontFamily().then((fontFamily) => {
      if (disposed) return;
      const currentPreferences = preferencesRef.current;
      terminal.options.fontFamily = fontFamily;
      terminal.options.fontSize = currentPreferences.fontSize;
      terminal.options.cursorStyle = currentPreferences.cursorStyle;
      terminal.options.cursorBlink = currentPreferences.cursorBlink;
      terminal.options.scrollback = currentPreferences.scrollback;
      terminal.open(mount);
      terminalRef.current = terminal;
      fitRef.current = fitAddon;
      void attachWebglRenderer(terminal, () => disposed);
      inputDisposable = terminal.onData(sendInput);
      binaryDisposable = terminal.onBinary((data) => {
        connectionRef.current?.sendBinary(data);
      });
      resizeObserver = new ResizeObserver(() => fit());
      resizeObserver.observe(mount);
      setTerminalReady(true);
      fit();
      if (activeRef.current) terminal.focus();
    });

    return () => {
      disposed = true;
      resizeObserver?.disconnect();
      inputDisposable?.dispose();
      binaryDisposable?.dispose();
      terminal.dispose();
      terminalRef.current = null;
      fitRef.current = null;
    };
  }, [fit, sendInput, session.id]);

  useEffect(() => {
    const terminal = terminalRef.current;
    if (!terminal) return;
    terminal.options.fontSize = preferences.fontSize;
    terminal.options.cursorStyle = preferences.cursorStyle;
    terminal.options.cursorBlink = preferences.cursorBlink;
    terminal.options.scrollback = preferences.scrollback;
    scheduleFit();
  }, [preferences, scheduleFit]);

  useEffect(() => {
    if (!active) return;
    window.setTimeout(() => {
      fit();
      terminalRef.current?.focus();
    }, ACTIVE_TAB_SETTLE_MS);
  }, [active, fit]);

  useEffect(() => {
    if (!terminalReady) return undefined;
    const connection = new TerminalConnection(session.id, {
      onOutput: (output, done) => {
        const terminal = terminalRef.current;
        // A disposed renderer still has to release the replay barrier.
        if (!terminal) {
          done?.();
          return;
        }
        terminal.write(output, done);
      },
      onStatus: setLiveStatus,
      onWriter: setWriter,
      onReplayComplete: setReplayComplete,
      onInputAllowed: (allowed) => {
        const terminal = terminalRef.current;
        if (terminal) terminal.options.disableStdin = !allowed;
      },
      onError: setLastError,
      onNotice: notify,
      onRefit: scheduleFit,
    });
    connectionRef.current = connection;
    connection.connect();

    return () => {
      connectionRef.current = null;
      connection.close();
    };
  }, [notify, scheduleFit, session.id, terminalReady]);

  /** Wake the server-side backoff first, then re-attach this browser. */
  async function retryBackendConnection() {
    setRetryingBackend(true);
    try {
      await api.reconnectSession(session.id);
    } catch (reason) {
      // A 404 only means the runtime is gone.
      if (!(reason instanceof ApiError && reason.status === 404)) {
        setLastError(errorMessage(reason));
        return;
      }
    } finally {
      setRetryingBackend(false);
    }
    connectionRef.current?.reconnect();
  }

  function sendSpecial(value: string) {
    sendInput(value);
    terminalRef.current?.focus();
  }

  function sendCursor(direction: CursorDirection) {
    const terminal = terminalRef.current;
    const value = encodeCursorKey(
      direction,
      terminal?.modes.applicationCursorKeysMode ?? false,
      ctrlRef.current,
      altRef.current,
    );
    clearModifiers();
    connectionRef.current?.send(value);
    terminal?.focus();
  }

  function availableClipboard(): Clipboard | null {
    if (!window.isSecureContext) {
      notify('当前来源不是安全上下文；剪贴板功能需要 HTTPS（localhost 或回环地址除外）。', 'error');
      return null;
    }
    if (!navigator.clipboard) {
      notify('当前浏览器不支持剪贴板 API，请使用系统复制或粘贴快捷键。', 'error');
      return null;
    }
    return navigator.clipboard;
  }

  async function copySelection() {
    const clipboard = availableClipboard();
    if (!clipboard) return;
    const value = terminalRef.current?.getSelection();
    if (!value) {
      notify('先在终端中选择要复制的内容', 'info');
      return;
    }
    try {
      await clipboard.writeText(value);
      notify('已复制选中内容', 'success');
    } catch {
      notify('浏览器未授予剪贴板权限', 'error');
    }
  }

  async function pasteClipboard() {
    if (writer === false) return;
    const clipboard = availableClipboard();
    if (!clipboard) return;
    if (!replayComplete) {
      notify('终端历史仍在同步，请稍后再粘贴。', 'info');
      return;
    }
    try {
      const value = await clipboard.readText();
      if (value) {
        clearModifiers();
        terminalRef.current?.paste(value);
      }
    } catch {
      notify('浏览器未授予剪贴板权限', 'error');
    }
    terminalRef.current?.focus();
  }

  function toggleCtrl() {
    ctrlRef.current = !ctrlRef.current;
    setCtrl(ctrlRef.current);
    terminalRef.current?.focus();
  }

  function toggleAlt() {
    altRef.current = !altRef.current;
    setAlt(altRef.current);
    terminalRef.current?.focus();
  }

  const statusClass =
    liveStatus === 'running'
      ? 'is-online'
      : liveStatus === 'error' || liveStatus === 'exited'
        ? 'is-error'
        : 'is-pending';

  return (
    <div
      className={`terminal-view ${active ? 'is-active' : ''}`}
      aria-hidden={!active}
      data-terminal-ready={terminalReady}
      data-replay-complete={replayComplete}
    >
      <header className="terminal-toolbar">
        <div className="terminal-identity">
          <span className={`connection-dot ${statusClass}`} />
          <div>
            <strong>{session.name}</strong>
            <span>
              {session.kind === 'local' ? <TerminalSquare size={13} /> : <Server size={13} />}
              {session.kind === 'local' ? '本机' : (session.hostName ?? 'SSH')}
              <i>·</i>
              {session.cwd || '~'}
            </span>
          </div>
        </div>
        <div className="terminal-toolbar__actions">
          <span className={`live-status ${statusClass}`} role="status" aria-live="polite" aria-atomic="true">
            {(liveStatus === 'connecting' || liveStatus === 'reconnecting') && (
              <LoaderCircle className="spin" size={13} />
            )}
            {liveStatusLabel(liveStatus)}
          </span>
          <button className="tool-button desktop-only" onClick={() => void copySelection()} title="复制选中内容">
            <Copy size={16} />
          </button>
          <button
            className="tool-button desktop-only"
            onClick={() => terminalRef.current?.clear()}
            title="清空可见内容"
          >
            <Eraser size={16} />
          </button>
          <button
            className="tool-button desktop-only"
            onClick={() => {
              if (document.fullscreenElement) void document.exitFullscreen();
              else void mountRef.current?.closest('.terminal-view')?.requestFullscreen();
            }}
            title="全屏"
          >
            <Maximize2 size={16} />
          </button>
          <button
            type="button"
            className="tool-button terminate-session-button"
            onClick={() => onTerminate(session)}
            title="结束会话"
            aria-label={`结束会话 ${session.name}`}
          >
            <Square size={16} />
          </button>
        </div>
      </header>

      <div className="terminal-canvas-wrap">
        <div ref={mountRef} className="terminal-canvas" onClick={() => terminalRef.current?.focus()} />

        {!terminalReady && (
          <div className="terminal-loader" role="status">
            <span />
            <p>正在载入终端字体…</p>
          </div>
        )}

        {terminalReady && !replayComplete && liveStatus === 'running' && (
          <div className="read-only-banner" role="status">
            <LoaderCircle className="spin" size={14} />
            <span>正在同步历史输出…</span>
          </div>
        )}

        {liveStatus === 'reconnecting' && (
          <div className="read-only-banner" role="status">
            <LoaderCircle className="spin" size={14} />
            <span>{lastError || '正在重新连接…'}</span>
            <button disabled={retryingBackend} onClick={() => void retryBackendConnection()}>
              重试后台连接
            </button>
          </div>
        )}

        {replayComplete && writer === false && liveStatus === 'running' && (
          <div className="read-only-banner">
            <Lock size={14} />
            <span>另一个设备正在输入</span>
            <button onClick={() => connectionRef.current?.takeControl()}>接管</button>
          </div>
        )}

        {(liveStatus === 'error' || liveStatus === 'exited') && (
          <div className="terminal-state-card">
            <span className="terminal-state-card__icon">
              <AlertTriangle size={22} />
            </span>
            <strong>{liveStatus === 'exited' ? '会话已经退出' : '终端连接中断'}</strong>
            <p>
              {lastError || (liveStatus === 'exited' ? '可以重启后继续使用这个会话。' : '检查 wmux 服务或网络连接。')}
            </p>
            <div>
              {liveStatus === 'exited' ? (
                <Button tone="primary" size="sm" busy={restarting} onClick={() => onRestart(session)}>
                  <RefreshCw size={15} /> 重启会话
                </Button>
              ) : (
                <Button tone="primary" size="sm" busy={retryingBackend} onClick={() => void retryBackendConnection()}>
                  <RefreshCw size={15} /> 重试后台连接
                </Button>
              )}
            </div>
          </div>
        )}
      </div>

      <nav className="special-keys" aria-label="终端特殊按键">
        <button className={ctrl ? 'is-active' : ''} onClick={toggleCtrl}>
          Ctrl
        </button>
        <button className={alt ? 'is-active' : ''} onClick={toggleAlt}>
          Alt
        </button>
        <button onClick={() => sendSpecial('\x1b')}>Esc</button>
        <button onClick={() => sendSpecial('\t')}>Tab</button>
        <button onClick={() => sendSpecial('|')}>|</button>
        <button onClick={() => sendSpecial('/')}>/</button>
        <button onClick={() => sendSpecial('-')}>-</button>
        <button aria-label="向左" onClick={() => sendCursor('left')}>
          <ArrowLeft size={16} />
        </button>
        <button aria-label="向下" onClick={() => sendCursor('down')}>
          <ArrowDown size={16} />
        </button>
        <button aria-label="向上" onClick={() => sendCursor('up')}>
          <ArrowUp size={16} />
        </button>
        <button aria-label="向右" onClick={() => sendCursor('right')}>
          <ArrowRight size={16} />
        </button>
        <button aria-label="复制选中内容" onClick={() => void copySelection()}>
          <Copy size={16} />
        </button>
        <button aria-label="粘贴" onClick={() => void pasteClipboard()}>
          <Clipboard size={16} />
        </button>
        <button aria-label="显示键盘" onClick={() => terminalRef.current?.focus()}>
          <Keyboard size={16} />
        </button>
      </nav>
    </div>
  );
}
