// @vitest-environment jsdom

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { AUTH_EXPIRED_EVENT } from './api';
import { TerminalConnection, type TerminalConnectionSink } from './terminalConnection';
import { OUTPUT_FRAME_HEADER_BYTES, SERVER_OUTPUT_FRAME, type LiveStatus } from './terminalProtocol';

const apiHarness = vi.hoisted(() => ({ status: vi.fn() }));

vi.mock('./api', async (importOriginal) => ({
  ...(await importOriginal<typeof import('./api')>()),
  api: apiHarness,
}));

type SocketEvent = { data?: unknown; code?: number };
type SocketListener = (event: SocketEvent) => void;

class FakeWebSocket {
  static readonly CONNECTING = 0;
  static readonly OPEN = 1;
  static readonly CLOSING = 2;
  static readonly CLOSED = 3;
  static instances: FakeWebSocket[] = [];

  readonly url: string;
  binaryType = '';
  readyState = FakeWebSocket.CONNECTING;
  sent: Array<string | ArrayBuffer> = [];
  closedWith: number | null = null;
  private listeners = new Map<string, Set<SocketListener>>();

  constructor(url: string | URL) {
    this.url = String(url);
    FakeWebSocket.instances.push(this);
  }

  addEventListener(type: string, listener: SocketListener): void {
    const listeners = this.listeners.get(type) ?? new Set<SocketListener>();
    listeners.add(listener);
    this.listeners.set(type, listeners);
  }

  send(value: string | ArrayBuffer): void {
    this.sent.push(value);
  }

  close(code?: number): void {
    this.readyState = FakeWebSocket.CLOSED;
    this.closedWith = code ?? null;
  }

  emitOpen(): void {
    this.readyState = FakeWebSocket.OPEN;
    this.emit('open', {});
  }

  emitMessage(data: string | ArrayBuffer): void {
    this.emit('message', { data });
  }

  emitClose(code = 1006): void {
    this.readyState = FakeWebSocket.CLOSED;
    this.emit('close', { code });
  }

  binaryFrames(): ArrayBuffer[] {
    return this.sent.filter((value): value is ArrayBuffer => value instanceof ArrayBuffer);
  }

  textFrames(): string[] {
    const decoder = new TextDecoder();
    return this.binaryFrames().map((frame) => decoder.decode(new Uint8Array(frame).subarray(1)));
  }

  private emit(type: string, event: SocketEvent): void {
    this.listeners.get(type)?.forEach((listener) => listener(event));
  }
}

function outputFrame(sequence: bigint, value: string): ArrayBuffer {
  const output = new TextEncoder().encode(value);
  const packet = new Uint8Array(new ArrayBuffer(OUTPUT_FRAME_HEADER_BYTES + output.length));
  packet[0] = SERVER_OUTPUT_FRAME;
  new DataView(packet.buffer).setBigUint64(1, sequence, false);
  packet.set(output, OUTPUT_FRAME_HEADER_BYTES);
  return packet.buffer;
}

type Recorder = {
  sink: TerminalConnectionSink;
  writes: Array<{ text: string; done: (() => void) | undefined }>;
  statuses: LiveStatus[];
  writers: Array<boolean | null>;
  replayComplete: boolean[];
  inputAllowed: boolean[];
  errors: string[];
  notices: Array<[string, string]>;
  refits: number;
  /** Simulates an xterm write that throws while parsing replay output. */
  rejectWrites: boolean;
};

function recordingSink(): Recorder {
  const decoder = new TextDecoder();
  const recorder: Recorder = {
    sink: {} as TerminalConnectionSink,
    writes: [],
    statuses: [],
    writers: [],
    replayComplete: [],
    inputAllowed: [],
    errors: [],
    notices: [],
    refits: 0,
    rejectWrites: false,
  };
  recorder.sink = {
    onOutput(output, done) {
      if (recorder.rejectWrites) throw new Error('unparsable output');
      recorder.writes.push({ text: decoder.decode(output), done });
    },
    onStatus: (status) => recorder.statuses.push(status),
    onWriter: (writer) => recorder.writers.push(writer),
    onReplayComplete: (complete) => recorder.replayComplete.push(complete),
    onInputAllowed: (allowed) => recorder.inputAllowed.push(allowed),
    onError: (message) => recorder.errors.push(message),
    onNotice: (message, tone) => recorder.notices.push([message, tone]),
    onRefit: () => {
      recorder.refits += 1;
    },
  };
  return recorder;
}

function lastSocket(): FakeWebSocket {
  const socket = FakeWebSocket.instances.at(-1);
  if (!socket) throw new Error('no socket was opened');
  return socket;
}

beforeEach(() => {
  FakeWebSocket.instances.length = 0;
  apiHarness.status.mockReset();
  apiHarness.status.mockResolvedValue({ setupRequired: false, authenticated: true, version: 'test' });
  vi.stubGlobal('WebSocket', FakeWebSocket);
});

afterEach(() => {
  vi.unstubAllGlobals();
  vi.useRealTimers();
});

describe('TerminalConnection replay barrier', () => {
  it('holds input until every replayed write drains', () => {
    const recorder = recordingSink();
    const connection = new TerminalConnection('session-1', recorder.sink);
    connection.connect();
    const socket = lastSocket();

    socket.emitOpen();
    socket.emitMessage('{"type":"hello","status":"running","writer":true}');
    socket.emitMessage(outputFrame(1n, 'history'));
    socket.emitMessage('{"type":"replay_end","sequence":1}');

    expect(connection.send('too early')).toBe(false);
    expect(recorder.replayComplete).toEqual([false]);
    expect(recorder.inputAllowed.at(-1)).toBe(false);

    const pending = recorder.writes.at(-1);
    expect(pending?.text).toBe('history');
    pending?.done?.();

    expect(recorder.replayComplete.at(-1)).toBe(true);
    expect(recorder.inputAllowed.at(-1)).toBe(true);
    expect(connection.send('ready')).toBe(true);
    expect(socket.textFrames()).toEqual(['ready']);
  });

  it('releases the barrier and reports an error when the renderer rejects a write', () => {
    const recorder = recordingSink();
    const connection = new TerminalConnection('session-1', recorder.sink);
    connection.connect();
    const socket = lastSocket();

    socket.emitOpen();
    socket.emitMessage('{"type":"hello","status":"running","writer":true}');
    recorder.rejectWrites = true;
    socket.emitMessage(outputFrame(1n, 'broken'));
    socket.emitMessage('{"type":"replay_end","sequence":1}');

    expect(recorder.errors).toContain('终端输出解析失败，请重新连接。');
    expect(recorder.statuses.at(-1)).toBe('error');
    expect(recorder.replayComplete.at(-1)).toBe(true);
  });

  it('writes live output without a completion callback once replay ended', () => {
    const recorder = recordingSink();
    const connection = new TerminalConnection('session-1', recorder.sink);
    connection.connect();
    const socket = lastSocket();

    socket.emitOpen();
    socket.emitMessage('{"type":"hello","status":"running","writer":true}');
    socket.emitMessage('{"type":"replay_end","sequence":0}');
    socket.emitMessage(outputFrame(2n, 'live'));

    expect(recorder.writes).toEqual([{ text: 'live', done: undefined }]);
  });
});

describe('TerminalConnection input gating', () => {
  it('refuses input until the server reports the session running', () => {
    const recorder = recordingSink();
    const connection = new TerminalConnection('session-1', recorder.sink);
    connection.connect();
    const socket = lastSocket();

    socket.emitOpen();
    socket.emitMessage('{"type":"hello","status":"connecting","writer":true}');
    socket.emitMessage('{"type":"replay_end","sequence":0}');
    expect(connection.send('too early')).toBe(false);

    socket.emitMessage('{"type":"state","status":"running"}');
    expect(connection.send('ready')).toBe(true);
    expect(socket.textFrames()).toEqual(['ready']);
  });

  it('locks input for a read-only viewer and asks the server to hand over control', () => {
    const recorder = recordingSink();
    const connection = new TerminalConnection('session-1', recorder.sink);
    connection.connect();
    const socket = lastSocket();

    socket.emitOpen();
    socket.emitMessage('{"type":"hello","status":"running","writer":false}');
    socket.emitMessage('{"type":"replay_end","sequence":0}');
    expect(recorder.inputAllowed.at(-1)).toBe(false);
    expect(connection.send('blocked')).toBe(false);

    connection.takeControl();
    expect(socket.sent).toContain('{"type":"take_control"}');

    socket.emitMessage('{"type":"writer","writer":true}');
    expect(recorder.inputAllowed.at(-1)).toBe(true);
    expect(connection.sendBinary(String.fromCharCode(0x80, 0xff))).toBe(true);
    expect([...new Uint8Array(socket.binaryFrames()[0]!)]).toEqual([0, 0x80, 0xff]);
  });

  it('reports the viewport size only while the socket is open', () => {
    const recorder = recordingSink();
    const connection = new TerminalConnection('session-1', recorder.sink);
    connection.connect();
    const socket = lastSocket();

    connection.resize(80, 24);
    expect(socket.sent).toHaveLength(0);

    socket.emitOpen();
    connection.resize(120, 40);
    expect(socket.sent).toEqual(['{"type":"resize","cols":120,"rows":40}']);
  });
});

describe('TerminalConnection reconnect handling', () => {
  it('resumes from the last sequence after a backoff', async () => {
    vi.useFakeTimers();
    const recorder = recordingSink();
    const connection = new TerminalConnection('session-1', recorder.sink);
    connection.connect();
    const socket = lastSocket();

    socket.emitOpen();
    socket.emitMessage('{"type":"hello","status":"running","writer":true}');
    socket.emitMessage(outputFrame(7n, 'output'));
    socket.emitClose(1006);

    await vi.advanceTimersByTimeAsync(999);
    expect(FakeWebSocket.instances).toHaveLength(1);
    await vi.advanceTimersByTimeAsync(1);
    expect(FakeWebSocket.instances).toHaveLength(2);
    expect(lastSocket().url).toContain('?since=7');
    expect(recorder.statuses.at(-1)).toBe('reconnecting');
  });

  it('ignores events from a socket that has already been replaced', () => {
    const recorder = recordingSink();
    const connection = new TerminalConnection('session-1', recorder.sink);
    connection.connect();
    const stale = lastSocket();
    connection.connect();

    stale.emitMessage('{"type":"state","status":"error"}');
    stale.emitClose(1006);

    expect(recorder.statuses).not.toContain('error');
    expect(FakeWebSocket.instances).toHaveLength(2);
  });

  it('reconnects when a restart disconnect follows a stale exited state', async () => {
    vi.useFakeTimers();
    const recorder = recordingSink();
    const connection = new TerminalConnection('session-1', recorder.sink);
    connection.connect();
    const socket = lastSocket();

    socket.emitOpen();
    socket.emitMessage('{"type":"hello","status":"running","writer":true}');
    socket.emitMessage('{"type":"state","status":"exited","writer":false}');
    socket.emitMessage('{"type":"disconnect","status":"reconnecting","reason":"restarted"}');
    expect(recorder.errors).toContain('会话已在其他设备重启，正在重新连接');

    socket.emitClose(1013);
    await vi.advanceTimersByTimeAsync(1000);
    expect(FakeWebSocket.instances).toHaveLength(2);
  });

  it('stops reconnecting after a policy close and reports the revoked login', async () => {
    vi.useFakeTimers();
    apiHarness.status.mockResolvedValue({ setupRequired: false, authenticated: false, version: 'test' });
    const expired = vi.fn();
    window.addEventListener(AUTH_EXPIRED_EVENT, expired);
    const recorder = recordingSink();
    const connection = new TerminalConnection('session-1', recorder.sink);
    connection.connect();
    const socket = lastSocket();

    socket.emitOpen();
    socket.emitMessage('{"type":"hello","status":"running","writer":true}');
    socket.emitClose(1008);
    await vi.advanceTimersByTimeAsync(8000);

    window.removeEventListener(AUTH_EXPIRED_EVENT, expired);
    expect(expired).toHaveBeenCalledOnce();
    expect(recorder.statuses.at(-1)).toBe('error');
    expect(recorder.errors).toContain('此会话当前不可连接，请刷新会话列表或检查会话配置。');
    expect(FakeWebSocket.instances).toHaveLength(1);
  });

  it('closes the transport for good and skips the pending backoff', async () => {
    vi.useFakeTimers();
    const recorder = recordingSink();
    const connection = new TerminalConnection('session-1', recorder.sink);
    connection.connect();
    const socket = lastSocket();

    socket.emitOpen();
    connection.close();
    expect(socket.closedWith).toBe(1000);

    socket.emitClose(1006);
    await vi.advanceTimersByTimeAsync(8000);
    expect(FakeWebSocket.instances).toHaveLength(1);
  });

  it('notifies once when the server dropped older replay output', () => {
    const recorder = recordingSink();
    const connection = new TerminalConnection('session-1', recorder.sink);
    connection.connect();
    const socket = lastSocket();

    socket.emitOpen();
    socket.emitMessage('{"type":"hello","status":"running","writer":true,"truncated":true}');
    expect(recorder.notices).toEqual([['较早的终端输出已被清理，当前显示最近记录', 'info']]);
  });
});
