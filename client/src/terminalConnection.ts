import { api, ApiError, signalAuthExpired } from './api';
import {
  decodeTerminalOutput,
  encodeTerminalBinaryFrames,
  encodeTerminalTextFrames,
  isPermanentSocketClose,
  normalizeLiveStatus,
  parseControlMessage,
  ReplayBarrier,
  websocketAddress,
  type LiveStatus,
} from './terminalProtocol';

function disconnectMessage(reason: string | undefined): string {
  if (reason === 'evicted') return '输出传输暂时落后，正在恢复连接。';
  // The server closes with 1013 after a restart.
  if (reason === 'restarted') return '会话已在其他设备重启，正在重新连接';
  return 'wmux 服务正在重启，正在恢复连接。';
}

/** Everything the transport pushes back into the view layer. */
export type TerminalConnectionSink = {
  /**
   * Render server output. While replay is still collecting, `done` is supplied
   * and has to run once the renderer has parsed the write; throwing instead
   * reports a renderer failure.
   */
  onOutput(output: Uint8Array<ArrayBuffer>, done?: () => void): void;
  onStatus(status: LiveStatus): void;
  /** `null` until the server reports which client holds the write lock. */
  onWriter(writer: boolean | null): void;
  onReplayComplete(complete: boolean): void;
  /** Whether the renderer should accept keystrokes at all. */
  onInputAllowed(allowed: boolean): void;
  /** Inline connection error text; an empty string clears it. */
  onError(message: string): void;
  onNotice(message: string, tone: 'error' | 'info'): void;
  /** Re-measure the viewport and report the new size back through `resize`. */
  onRefit(): void;
};

/**
 * Owns the terminal WebSocket: reconnect backoff, the replay barrier, the write
 * lock and input framing. It has no React dependency so the view only has to
 * render what the sink reports.
 */
export class TerminalConnection {
  private readonly replayBarrier = new ReplayBarrier();
  private socket: WebSocket | null = null;
  private lastSequence = 0;
  private reconnectTimer: number | null = null;
  private reconnectAttempts = 0;
  private shouldReconnect = true;
  private status: LiveStatus = 'connecting';
  private writer: boolean | null = null;

  constructor(
    private readonly sessionId: string,
    private readonly sink: TerminalConnectionSink,
  ) {}

  connect(): void {
    this.clearReconnectTimer();
    const current = this.socket;
    if (current && (current.readyState === WebSocket.OPEN || current.readyState === WebSocket.CONNECTING))
      current.close();
    this.resetReplay();
    this.setWriter(null);
    this.updateStatus(this.reconnectAttempts ? 'reconnecting' : 'connecting');
    this.sink.onError('');

    const socket = new WebSocket(websocketAddress(this.sessionId, this.lastSequence));
    socket.binaryType = 'arraybuffer';
    this.socket = socket;

    socket.addEventListener('open', () => {
      // A late listener from a replaced socket must not touch the live state.
      if (this.socket !== socket) return;
      this.reconnectAttempts = 0;
      this.sink.onRefit();
    });

    socket.addEventListener('message', (event) => {
      if (this.socket !== socket) return;
      this.handleMessage(event);
    });

    socket.addEventListener('close', (event) => {
      if (this.socket !== socket) return;
      this.handleClose(event);
    });

    socket.addEventListener('error', () => {
      if (this.socket !== socket) return;
      this.sink.onError('实时连接不可用，正在重试');
    });
  }

  /** Send terminal text; returns whether the transport accepted it. */
  send(value: string): boolean {
    return this.sendFrames(encodeTerminalTextFrames(value));
  }

  /** Send xterm's onBinary payload as raw 8-bit bytes. */
  sendBinary(value: string): boolean {
    return this.sendFrames(encodeTerminalBinaryFrames(value));
  }

  resize(cols: number, rows: number): void {
    const socket = this.socket;
    if (socket?.readyState !== WebSocket.OPEN) return;
    socket.send(JSON.stringify({ type: 'resize', cols, rows }));
  }

  takeControl(): void {
    const socket = this.socket;
    if (!socket || socket.readyState !== WebSocket.OPEN) return;
    socket.send(JSON.stringify({ type: 'take_control' }));
  }

  /** Restart the transport from a user gesture, skipping the backoff. */
  reconnect(): void {
    this.shouldReconnect = true;
    this.reconnectAttempts = 0;
    const socket = this.socket;
    if (socket && socket.readyState < WebSocket.CLOSING) socket.close(4000, 'manual reconnect');
    else this.connect();
  }

  close(): void {
    this.shouldReconnect = false;
    this.clearReconnectTimer();
    this.replayBarrier.reset();
    const socket = this.socket;
    this.socket = null;
    if (socket && socket.readyState < WebSocket.CLOSING) socket.close(1000, 'view closed');
  }

  private handleMessage(event: MessageEvent): void {
    if (event.data instanceof ArrayBuffer) {
      this.handleOutputFrame(event.data);
      return;
    }
    if (typeof event.data !== 'string') return;
    const message = parseControlMessage(event.data);
    if (!message) return;

    const status = normalizeLiveStatus(message.status);
    if (status) {
      this.updateStatus(status);
      if (status === 'exited') this.shouldReconnect = false;
    }
    if (typeof message.sequence === 'number' && message.sequence > this.lastSequence) {
      this.lastSequence = message.sequence;
    }
    if (
      message.type === 'writer' ||
      message.type === 'hello' ||
      message.type === 'state' ||
      message.type === 'replay_end'
    ) {
      const writable = message.writer;
      if (typeof writable === 'boolean') {
        this.setWriter(writable);
        if (writable) this.sink.onRefit();
      }
    }
    if (message.type === 'replay_end' && this.replayBarrier.endReplay()) this.finishReplay();
    if (message.type === 'hello' && message.truncated) {
      this.sink.onNotice('较早的终端输出已被清理，当前显示最近记录', 'info');
    }
    if (message.type === 'disconnect') {
      // An explicit reconnect instruction outranks an earlier exited status.
      if (status === 'reconnecting') this.shouldReconnect = true;
      this.sink.onError(disconnectMessage(message.reason));
    }
    if (status === 'error') this.sink.onError('终端暂时不可用，请检查会话配置或主机连接。');
    if (message.type === 'error') {
      // The server keeps the connection open for these.
      const text = message.message || '终端连接发生错误。';
      this.sink.onError(text);
      this.sink.onNotice(text, 'error');
    }
  }

  private handleOutputFrame(data: ArrayBuffer): void {
    const frame = decodeTerminalOutput(data);
    if (!frame) return;
    if (frame.sequence > this.lastSequence) this.lastSequence = frame.sequence;
    if (!frame.output.length) return;
    if (!this.replayBarrier.isCollectingReplay()) {
      // xterm queues this behind the replay writes, so the barrier opens first.
      this.sink.onOutput(frame.output);
      return;
    }

    const completeWrite = this.replayBarrier.trackReplayWrite();
    try {
      this.sink.onOutput(frame.output, () => {
        if (completeWrite()) this.finishReplay();
      });
    } catch {
      // A rejected write still has to release the replay barrier.
      if (completeWrite()) this.finishReplay();
      this.sink.onError('终端输出解析失败，请重新连接。');
      this.updateStatus('error');
    }
  }

  private handleClose(event: CloseEvent): void {
    this.socket = null;
    this.resetReplay();
    // A revoked login arrives as an exited disconnect, so 1008 is checked first.
    if (isPermanentSocketClose(event.code)) {
      this.shouldReconnect = false;
      this.setWriter(false);
      if (this.status !== 'exited') {
        this.updateStatus('error');
        this.sink.onError('此会话当前不可连接，请刷新会话列表或检查会话配置。');
      }
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
    if (!this.shouldReconnect || this.status === 'exited') {
      return;
    }
    void api
      .status()
      .then((status) => {
        if (!this.shouldReconnect) return;
        if (!status.authenticated) {
          this.shouldReconnect = false;
          signalAuthExpired();
          return;
        }
        this.setWriter(null);
        this.reconnectAttempts += 1;
        this.updateStatus('reconnecting');
        const delay = Math.min(8000, 500 * 2 ** Math.min(this.reconnectAttempts, 4));
        this.reconnectTimer = window.setTimeout(() => this.connect(), delay);
      })
      .catch((reason: unknown) => {
        if (!this.shouldReconnect) return;
        if (reason instanceof ApiError && reason.status === 401) {
          this.shouldReconnect = false;
          signalAuthExpired();
          return;
        }
        this.reconnectAttempts += 1;
        this.updateStatus('reconnecting');
        this.reconnectTimer = window.setTimeout(() => this.connect(), 2000);
      });
  }

  private sendFrames(frames: ArrayBuffer[]): boolean {
    const socket = this.socket;
    if (!this.canSendInput() || !socket) return false;
    for (const frame of frames) socket.send(frame);
    return true;
  }

  private canSendInput(): boolean {
    const socket = this.socket;
    // The backend accepts input only after the server reports it running.
    return (
      this.replayBarrier.isOpen() &&
      this.writer !== false &&
      this.status === 'running' &&
      Boolean(socket && socket.readyState === WebSocket.OPEN)
    );
  }

  private inputAllowed(): boolean {
    return this.writer !== false && this.replayBarrier.isOpen();
  }

  private updateStatus(status: LiveStatus): void {
    this.status = status;
    this.sink.onStatus(status);
  }

  private setWriter(writer: boolean | null): void {
    this.writer = writer;
    this.sink.onWriter(writer);
    this.sink.onInputAllowed(this.inputAllowed());
  }

  private resetReplay(): void {
    this.replayBarrier.reset();
    this.sink.onReplayComplete(false);
    this.sink.onInputAllowed(false);
  }

  private finishReplay(): void {
    this.sink.onInputAllowed(this.inputAllowed());
    this.sink.onReplayComplete(true);
    this.sink.onRefit();
  }

  private clearReconnectTimer(): void {
    if (this.reconnectTimer !== null) {
      window.clearTimeout(this.reconnectTimer);
      this.reconnectTimer = null;
    }
  }
}
