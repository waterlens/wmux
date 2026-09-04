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
  Check,
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
import { api, ApiError, signalAuthExpired } from '../api';
import {
  applyTerminalModifiers,
  decodeTerminalOutput,
  encodeCursorKey,
  encodeTerminalBinaryFrames,
  encodeTerminalTextFrames,
  isPermanentSocketClose,
  normalizeLiveStatus,
  parseControlMessage,
  ReplayBarrier,
  websocketAddress,
  type CursorDirection,
  type LiveStatus,
} from '../terminalProtocol';
import { openTerminalAfterFonts, TERMINAL_SYSTEM_FONT_FAMILY } from '../terminalFonts';
import type { Session, TerminalPreferences } from '../types';
import { Button } from './UI';

type TerminalViewProps = {
  session: Session;
  active: boolean;
  preferences: TerminalPreferences;
  onRestart: (session: Session) => void;
  onTerminate: (session: Session) => void;
  notify: (message: string, tone?: 'success' | 'error' | 'info') => void;
};

const statusText: Record<LiveStatus, string> = {
  connecting: '正在连接',
  running: '已连接',
  reconnecting: '正在重连',
  detached: '已分离',
  exited: '已退出',
  error: '连接错误',
  offline: '已断开',
};

export function TerminalView({ session, active, preferences, onRestart, onTerminate, notify }: TerminalViewProps) {
  const mountRef = useRef<HTMLDivElement>(null);
  const terminalRef = useRef<Terminal | null>(null);
  const fitRef = useRef<FitAddon | null>(null);
  const socketRef = useRef<WebSocket | null>(null);
  const lastSequenceRef = useRef(0);
  const reconnectTimerRef = useRef<number | null>(null);
  const reconnectAttemptsRef = useRef(0);
  const shouldReconnectRef = useRef(true);
  const activeRef = useRef(active);
  const liveStatusRef = useRef<LiveStatus>('connecting');
  const writerRef = useRef<boolean | null>(null);
  const ctrlRef = useRef(false);
  const altRef = useRef(false);
  const sendInputRef = useRef<(value: string) => void>(() => undefined);
  const sendBinaryRef = useRef<(value: string) => void>(() => undefined);
  const sendExactInputRef = useRef<(value: string) => void>(() => undefined);
  const connectRef = useRef<() => void>(() => undefined);
  const preferencesRef = useRef(preferences);
  const [replayBarrier] = useState(() => new ReplayBarrier());
  const [liveStatus, setLiveStatus] = useState<LiveStatus>('connecting');
  const [writer, setWriter] = useState<boolean | null>(null);
  const [ctrl, setCtrl] = useState(false);
  const [alt, setAlt] = useState(false);
  const [lastError, setLastError] = useState('');
  const [terminalReady, setTerminalReady] = useState(false);
  const [replayComplete, setReplayComplete] = useState(false);

  useEffect(() => {
    activeRef.current = active;
  }, [active]);

  useEffect(() => {
    preferencesRef.current = preferences;
  }, [preferences]);

  const setWriterState = useCallback(
    (value: boolean | null) => {
      writerRef.current = value;
      setWriter(value);
      if (terminalRef.current) terminalRef.current.options.disableStdin = value === false || !replayBarrier.isOpen();
    },
    [replayBarrier],
  );

  const updateStatus = useCallback((status: LiveStatus) => {
    liveStatusRef.current = status;
    setLiveStatus(status);
  }, []);

  const fit = useCallback(() => {
    const mount = mountRef.current;
    const terminal = terminalRef.current;
    const addon = fitRef.current;
    if (!mount || !terminal || !addon || !activeRef.current || mount.clientWidth < 20 || mount.clientHeight < 20)
      return;
    try {
      addon.fit();
      const socket = socketRef.current;
      if (socket?.readyState === WebSocket.OPEN && terminal.cols > 0 && terminal.rows > 0) {
        socket.send(JSON.stringify({ type: 'resize', cols: terminal.cols, rows: terminal.rows }));
      }
    } catch {
      // A transient zero-sized container can make fit fail during mobile rotation.
    }
  }, []);

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

    void openTerminalAfterFonts(
      (fontFamily) => {
        const currentPreferences = preferencesRef.current;
        terminal.options.fontFamily = fontFamily;
        terminal.options.fontSize = currentPreferences.fontSize;
        terminal.options.cursorStyle = currentPreferences.cursorStyle;
        terminal.options.cursorBlink = currentPreferences.cursorBlink;
        terminal.options.scrollback = currentPreferences.scrollback;
        terminal.open(mount);
        terminalRef.current = terminal;
        fitRef.current = fitAddon;
        inputDisposable = terminal.onData((data) => sendInputRef.current(data));
        binaryDisposable = terminal.onBinary((data) => sendBinaryRef.current(data));
        resizeObserver = new ResizeObserver(() => fit());
        resizeObserver.observe(mount);
        setTerminalReady(true);
      },
      () => {
        fit();
        if (activeRef.current) terminal.focus();
      },
      () => disposed,
    );

    return () => {
      disposed = true;
      resizeObserver?.disconnect();
      inputDisposable?.dispose();
      binaryDisposable?.dispose();
      terminal.dispose();
      terminalRef.current = null;
      fitRef.current = null;
    };
  }, [fit, session.id]);

  useEffect(() => {
    const terminal = terminalRef.current;
    if (!terminal) return;
    terminal.options.fontSize = preferences.fontSize;
    terminal.options.cursorStyle = preferences.cursorStyle;
    terminal.options.cursorBlink = preferences.cursorBlink;
    terminal.options.scrollback = preferences.scrollback;
    window.setTimeout(fit, 0);
  }, [fit, preferences]);

  useEffect(() => {
    if (active) {
      window.setTimeout(() => {
        fit();
        terminalRef.current?.focus();
      }, 30);
    }
  }, [active, fit]);

  useEffect(() => {
    if (!terminalReady) return undefined;
    shouldReconnectRef.current = true;

    function clearReconnectTimer() {
      if (reconnectTimerRef.current !== null) {
        window.clearTimeout(reconnectTimerRef.current);
        reconnectTimerRef.current = null;
      }
    }

    function finishReplay() {
      const terminal = terminalRef.current;
      if (terminal) terminal.options.disableStdin = writerRef.current === false;
      setReplayComplete(true);
      window.setTimeout(fit, 0);
    }

    function resetReplay() {
      replayBarrier.reset();
      setReplayComplete(false);
      const terminal = terminalRef.current;
      if (terminal) terminal.options.disableStdin = true;
    }

    function canSendInput(): boolean {
      const socket = socketRef.current;
      return (
        replayBarrier.isOpen() && writerRef.current !== false && Boolean(socket && socket.readyState === WebSocket.OPEN)
      );
    }

    function sendFrames(frames: ArrayBuffer[]) {
      const socket = socketRef.current;
      if (!canSendInput() || !socket) return;
      for (const frame of frames) socket.send(frame);
    }

    function clearOneShotModifiers() {
      if (ctrlRef.current) {
        ctrlRef.current = false;
        setCtrl(false);
      }
      if (altRef.current) {
        altRef.current = false;
        setAlt(false);
      }
    }

    function connect() {
      clearReconnectTimer();
      const current = socketRef.current;
      if (current && (current.readyState === WebSocket.OPEN || current.readyState === WebSocket.CONNECTING))
        current.close();
      resetReplay();
      setWriterState(null);
      updateStatus(reconnectAttemptsRef.current ? 'reconnecting' : 'connecting');
      setLastError('');

      const socket = new WebSocket(websocketAddress(session.id, lastSequenceRef.current));
      socket.binaryType = 'arraybuffer';
      socketRef.current = socket;

      socket.addEventListener('open', () => {
        if (socketRef.current !== socket) return;
        reconnectAttemptsRef.current = 0;
        updateStatus('running');
        fit();
      });

      socket.addEventListener('message', (event) => {
        if (socketRef.current !== socket) return;
        if (event.data instanceof ArrayBuffer) {
          const frame = decodeTerminalOutput(event.data);
          if (!frame) return;
          if (frame.sequence > lastSequenceRef.current) lastSequenceRef.current = frame.sequence;
          const terminal = terminalRef.current;
          if (!frame.output.length || !terminal) return;
          if (replayBarrier.isCollectingReplay()) {
            const completeWrite = replayBarrier.trackReplayWrite();
            try {
              terminal.write(frame.output, () => {
                if (completeWrite()) finishReplay();
              });
            } catch {
              // Roll the pending count back even when xterm rejects a write;
              // otherwise this connection could remain read-only forever.
              if (completeWrite()) finishReplay();
              setLastError('终端输出解析失败，请重新连接。');
              updateStatus('error');
            }
          } else {
            // xterm queues this behind all replay writes. Their callbacks open
            // the barrier before this live frame can trigger device replies.
            terminal.write(frame.output);
          }
          return;
        }

        if (typeof event.data !== 'string') return;
        const message = parseControlMessage(event.data);
        if (message) {
          const status = normalizeLiveStatus(message.status);
          if (status) {
            updateStatus(status);
            if (status === 'exited') shouldReconnectRef.current = false;
          }
          if (typeof message.sequence === 'number' && message.sequence > lastSequenceRef.current) {
            lastSequenceRef.current = message.sequence;
          }
          if (
            message.type === 'writer' ||
            message.type === 'hello' ||
            message.type === 'state' ||
            message.type === 'replay_end'
          ) {
            const writable = message.writer;
            if (typeof writable === 'boolean') {
              setWriterState(writable);
              if (writable) window.setTimeout(fit, 0);
            }
          }
          if (message.type === 'replay_end' && replayBarrier.endReplay()) finishReplay();
          if (message.type === 'hello' && message.truncated) {
            notify('较早的终端输出已被清理，当前显示最近记录', 'info');
          }
          if (message.type === 'disconnect') {
            setLastError(
              message.reason === 'evicted' ? '输出传输暂时落后，正在恢复连接。' : 'wmux 服务正在重启，正在恢复连接。',
            );
          }
          if (status === 'error') setLastError('终端暂时不可用，请检查会话配置或主机连接。');
          if (message.type === 'error') {
            setLastError('终端连接发生错误。');
            updateStatus('error');
          }
        }
      });

      socket.addEventListener('close', (event) => {
        if (socketRef.current !== socket) return;
        socketRef.current = null;
        resetReplay();
        if (!shouldReconnectRef.current || liveStatusRef.current === 'exited') {
          return;
        }
        if (isPermanentSocketClose(event.code)) {
          shouldReconnectRef.current = false;
          setWriterState(false);
          updateStatus('error');
          setLastError('此会话当前不可连接，请刷新会话列表或检查会话配置。');
          void api
            .status()
            .then((status) => {
              if (!status.authenticated) signalAuthExpired();
            })
            .catch((reason: unknown) => {
              if (reason instanceof ApiError && reason.status === 401) signalAuthExpired();
            });
          return;
        }
        void api
          .status()
          .then((status) => {
            if (!shouldReconnectRef.current) return;
            if (!status.authenticated) {
              shouldReconnectRef.current = false;
              signalAuthExpired();
              return;
            }
            setWriterState(null);
            reconnectAttemptsRef.current += 1;
            updateStatus('reconnecting');
            const delay = Math.min(8000, 500 * 2 ** Math.min(reconnectAttemptsRef.current, 4));
            reconnectTimerRef.current = window.setTimeout(connect, delay);
          })
          .catch((reason: unknown) => {
            if (!shouldReconnectRef.current) return;
            if (reason instanceof ApiError && reason.status === 401) {
              shouldReconnectRef.current = false;
              signalAuthExpired();
              return;
            }
            reconnectAttemptsRef.current += 1;
            updateStatus('reconnecting');
            reconnectTimerRef.current = window.setTimeout(connect, 2000);
          });
      });

      socket.addEventListener('error', () => {
        if (socketRef.current !== socket) return;
        setLastError('实时连接不可用，正在重试');
      });
    }

    connectRef.current = connect;

    sendInputRef.current = (value: string) => {
      // During replay this can be a device reply generated by historical
      // output. Bail out before encoding or consuming sticky UI modifiers.
      if (!canSendInput()) return;
      const output = applyTerminalModifiers(value, ctrlRef.current, altRef.current);
      clearOneShotModifiers();
      sendFrames(encodeTerminalTextFrames(output));
    };
    sendExactInputRef.current = (value: string) => {
      if (canSendInput()) sendFrames(encodeTerminalTextFrames(value));
    };
    sendBinaryRef.current = (value: string) => {
      if (canSendInput()) sendFrames(encodeTerminalBinaryFrames(value));
    };

    connect();
    return () => {
      shouldReconnectRef.current = false;
      connectRef.current = () => undefined;
      sendInputRef.current = () => undefined;
      sendExactInputRef.current = () => undefined;
      sendBinaryRef.current = () => undefined;
      clearReconnectTimer();
      replayBarrier.reset();
      const socket = socketRef.current;
      socketRef.current = null;
      if (socket && socket.readyState < WebSocket.CLOSING) socket.close(1000, 'view closed');
    };
  }, [fit, notify, replayBarrier, session.id, setWriterState, terminalReady, updateStatus]);

  function manualReconnect() {
    shouldReconnectRef.current = true;
    reconnectAttemptsRef.current = 0;
    const socket = socketRef.current;
    if (socket && socket.readyState < WebSocket.CLOSING) socket.close(4000, 'manual reconnect');
    else connectRef.current();
  }

  function takeControl() {
    const socket = socketRef.current;
    if (!socket || socket.readyState !== WebSocket.OPEN) return;
    socket.send(JSON.stringify({ type: 'take_control' }));
  }

  function sendSpecial(value: string) {
    sendInputRef.current(value);
    terminalRef.current?.focus();
  }

  function clearToolbarModifiers() {
    ctrlRef.current = false;
    altRef.current = false;
    setCtrl(false);
    setAlt(false);
  }

  function sendCursor(direction: CursorDirection) {
    const terminal = terminalRef.current;
    const value = encodeCursorKey(
      direction,
      terminal?.modes.applicationCursorKeysMode ?? false,
      ctrlRef.current,
      altRef.current,
    );
    clearToolbarModifiers();
    sendExactInputRef.current(value);
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
        clearToolbarModifiers();
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
          <span className={`live-status ${statusClass}`}>
            {(liveStatus === 'connecting' || liveStatus === 'reconnecting') && (
              <LoaderCircle className="spin" size={13} />
            )}
            {liveStatus === 'running' && <Check size={13} />}
            {(liveStatus === 'error' || liveStatus === 'exited') && <AlertTriangle size={13} />}
            {statusText[liveStatus]}
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
          <Button
            className="terminate-session-button"
            size="sm"
            tone="danger"
            onClick={() => onTerminate(session)}
            aria-label={`结束会话 ${session.name}`}
          >
            <Square size={13} />
            <span>结束</span>
          </Button>
        </div>
      </header>

      <div className="terminal-canvas-wrap">
        <div
          ref={mountRef}
          className="terminal-canvas"
          data-terminal-ready={terminalReady}
          data-replay-complete={replayComplete}
          onClick={() => terminalRef.current?.focus()}
        />

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

        {replayComplete && writer === false && liveStatus === 'running' && (
          <div className="read-only-banner">
            <Lock size={14} />
            <span>另一个设备正在输入</span>
            <button onClick={takeControl}>接管</button>
          </div>
        )}

        {(liveStatus === 'error' || liveStatus === 'offline' || liveStatus === 'exited') && (
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
                <Button tone="primary" size="sm" onClick={() => onRestart(session)}>
                  <RefreshCw size={15} /> 重启会话
                </Button>
              ) : (
                <Button tone="primary" size="sm" onClick={manualReconnect}>
                  <RefreshCw size={15} /> 立即重连
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
