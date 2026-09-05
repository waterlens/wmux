// @vitest-environment jsdom

import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { ApiError } from '../api';
import { MAX_INPUT_FRAME_BYTES, OUTPUT_FRAME_HEADER_BYTES, SERVER_OUTPUT_FRAME } from '../terminalProtocol';
import type { Session, TerminalPreferences } from '../types';
import { TerminalView } from './TerminalView';

const fontHarness = vi.hoisted(() => ({ loadFonts: vi.fn() }));

const xtermHarness = vi.hoisted(() => {
  type DataHandler = (value: string) => void;

  class FakeTerminal {
    static instances: FakeTerminal[] = [];

    options: Record<string, unknown>;
    cols = 80;
    rows = 24;
    modes = { applicationCursorKeysMode: false };
    unicode = { activeVersion: '6' };
    dataHandler: DataHandler = () => undefined;
    binaryHandler: DataHandler = () => undefined;
    writeCallbacks: Array<(() => void) | undefined> = [];
    open = vi.fn();
    focus = vi.fn();
    clear = vi.fn();
    dispose = vi.fn();
    getSelection = vi.fn(() => 'selected');
    paste = vi.fn((value: string) => this.dataHandler(value));

    constructor(options: Record<string, unknown>) {
      this.options = { ...options };
      FakeTerminal.instances.push(this);
    }

    loadAddon = vi.fn();

    onData(handler: DataHandler): { dispose(): void } {
      this.dataHandler = handler;
      return { dispose: () => undefined };
    }

    onBinary(handler: DataHandler): { dispose(): void } {
      this.binaryHandler = handler;
      return { dispose: () => undefined };
    }

    write(_value: string | Uint8Array, callback?: () => void): void {
      this.writeCallbacks.push(callback);
    }

    emitData(value: string): void {
      this.dataHandler(value);
    }

    emitBinary(value: string): void {
      this.binaryHandler(value);
    }

    drainWrite(index = 0): void {
      this.writeCallbacks[index]?.();
    }
  }

  class FakeFitAddon {
    fit = vi.fn();
  }

  class FakeUnicode11Addon {}
  class FakeWebLinksAddon {}

  return { FakeFitAddon, FakeTerminal, FakeUnicode11Addon, FakeWebLinksAddon };
});

const apiHarness = vi.hoisted(() => ({
  status: vi.fn(async () => ({ setupRequired: false, authenticated: true, version: 'test' })),
  reconnectSession: vi.fn(async (): Promise<void> => undefined),
}));

vi.mock('../api', async (importOriginal) => ({
  ...(await importOriginal<typeof import('../api')>()),
  api: apiHarness,
}));

vi.mock('@xterm/addon-web-fonts', () => ({ loadFonts: fontHarness.loadFonts }));
vi.mock('@xterm/xterm', () => ({ Terminal: xtermHarness.FakeTerminal }));
vi.mock('@xterm/addon-fit', () => ({ FitAddon: xtermHarness.FakeFitAddon }));
vi.mock('@xterm/addon-unicode11', () => ({ Unicode11Addon: xtermHarness.FakeUnicode11Addon }));
vi.mock('@xterm/addon-web-links', () => ({ WebLinksAddon: xtermHarness.FakeWebLinksAddon }));
// jsdom has no WebGL context; the renderer choice itself is not under test.
vi.mock('@xterm/addon-webgl', () => ({
  WebglAddon: class {
    onContextLoss(): void {}
  },
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

  close(): void {
    this.readyState = FakeWebSocket.CLOSED;
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

  private emit(type: string, event: SocketEvent): void {
    this.listeners.get(type)?.forEach((listener) => listener(event));
  }
}

class FakeResizeObserver {
  observe(): void {}
  disconnect(): void {}
}

const session: Session = {
  id: 'session-1',
  name: '测试会话',
  kind: 'local',
  cwd: '~',
  persistence: 'auto',
  status: 'running',
  cols: 80,
  rows: 24,
  createdAt: '2026-01-01T00:00:00Z',
  updatedAt: '2026-01-01T00:00:00Z',
};

const preferences: TerminalPreferences = {
  fontSize: 14,
  cursorStyle: 'block',
  cursorBlink: true,
  scrollback: 10_000,
  theme: 'light',
};

function outputFrame(sequence: bigint, value: string): ArrayBuffer {
  const output = new TextEncoder().encode(value);
  const packet = new Uint8Array(new ArrayBuffer(OUTPUT_FRAME_HEADER_BYTES + output.length));
  packet[0] = SERVER_OUTPUT_FRAME;
  new DataView(packet.buffer).setBigUint64(1, sequence, false);
  packet.set(output, OUTPUT_FRAME_HEADER_BYTES);
  return packet.buffer;
}

function renderTerminal(notify = vi.fn(), onTerminate = vi.fn()) {
  return {
    notify,
    onTerminate,
    ...render(
      <TerminalView
        session={session}
        active
        preferences={preferences}
        onRestart={() => undefined}
        onTerminate={onTerminate}
        notify={notify}
      />,
    ),
  };
}

let releaseFonts: (faces: FontFace[]) => void;

beforeEach(() => {
  xtermHarness.FakeTerminal.instances.length = 0;
  apiHarness.status.mockReset();
  apiHarness.status.mockResolvedValue({ setupRequired: false, authenticated: true, version: 'test' });
  apiHarness.reconnectSession.mockReset();
  apiHarness.reconnectSession.mockResolvedValue(undefined);
  FakeWebSocket.instances.length = 0;
  vi.stubGlobal('WebSocket', FakeWebSocket);
  vi.stubGlobal('ResizeObserver', FakeResizeObserver);
  Object.defineProperty(window, 'isSecureContext', { configurable: true, value: true });
  Object.defineProperty(navigator, 'clipboard', {
    configurable: true,
    value: { readText: vi.fn(async () => ''), writeText: vi.fn(async () => undefined) },
  });
  const fonts = new Promise<FontFace[]>((resolve) => {
    releaseFonts = resolve;
  });
  fontHarness.loadFonts.mockReset();
  fontHarness.loadFonts.mockImplementation(() => fonts);
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

async function finishFontLoading(): Promise<void> {
  await act(async () => {
    releaseFonts([]);
    await Promise.resolve();
  });
  await waitFor(() =>
    expect(screen.getByText('测试会话').closest('.terminal-view')?.getAttribute('data-terminal-ready')).toBe('true'),
  );
}

describe('TerminalView rendering and input surface', () => {
  it('uses a quiet status indicator and an accessible icon-only terminate tool', async () => {
    const onTerminate = vi.fn();
    const { container } = renderTerminal(vi.fn(), onTerminate);

    const identity = container.querySelector('.terminal-identity');
    const status = container.querySelector('.live-status');
    expect(identity?.querySelector('.connection-dot')).not.toBeNull();
    expect(identity?.textContent).toContain('测试会话');
    expect(status?.textContent).toBe('启动中');
    expect(status?.querySelector('.spin')).not.toBeNull();
    expect(status?.getAttribute('role')).toBe('status');
    expect(status?.getAttribute('aria-live')).toBe('polite');
    expect(status?.getAttribute('aria-atomic')).toBe('true');

    const terminate = screen.getByRole('button', { name: '结束会话 测试会话' });
    expect(terminate.getAttribute('title')).toBe('结束会话');
    expect(terminate.classList.contains('tool-button')).toBe(true);
    expect(terminate.classList.contains('terminate-session-button')).toBe(true);
    expect(terminate.textContent).toBe('');
    fireEvent.click(terminate);
    expect(onTerminate).toHaveBeenCalledWith(session);

    await finishFontLoading();
    const socket = FakeWebSocket.instances[0];
    if (!socket) throw new Error('terminal transport was not initialized');

    act(() => socket.emitOpen());
    expect(status?.textContent).toBe('启动中');

    // The toolbar reports this browser's link, so a running session reads 已连接.
    act(() => socket.emitMessage('{"type":"hello","status":"running","writer":true}'));
    expect(status?.textContent).toBe('已连接');
    expect(status?.querySelector('svg')).toBeNull();

    act(() => socket.emitMessage('{"type":"state","status":"error"}'));
    expect(status?.textContent).toBe('异常');
    expect(status?.querySelector('svg')).toBeNull();

    act(() => socket.emitMessage('{"type":"state","status":"exited"}'));
    expect(status?.textContent).toBe('已结束');
    expect(status?.querySelector('svg')).toBeNull();
  });

  it('waits for webfonts before opening xterm or creating the WebSocket', async () => {
    renderTerminal();
    const terminal = xtermHarness.FakeTerminal.instances[0];
    expect(terminal).toBeDefined();
    expect(terminal?.open).not.toHaveBeenCalled();
    expect(FakeWebSocket.instances).toHaveLength(0);

    await finishFontLoading();
    expect(terminal?.open).toHaveBeenCalledOnce();
    expect(FakeWebSocket.instances).toHaveLength(1);
    expect(terminal?.unicode.activeVersion).toBe('11');
    expect(terminal?.options.allowProposedApi).toBe(true);
    expect(terminal?.options.theme).toBeUndefined();
  });

  it('suppresses replay replies until write callbacks drain, then sends text, binary, and paste safely', async () => {
    const largePaste = '你😀'.repeat(30_000);
    const readText = vi.fn(async () => largePaste);
    Object.defineProperty(navigator, 'clipboard', {
      configurable: true,
      value: { readText, writeText: vi.fn(async () => undefined) },
    });
    renderTerminal();
    await finishFontLoading();

    const terminal = xtermHarness.FakeTerminal.instances[0];
    const socket = FakeWebSocket.instances[0];
    expect(terminal).toBeDefined();
    expect(socket).toBeDefined();
    if (!terminal || !socket) throw new Error('terminal transport was not initialized');

    act(() => {
      socket.emitOpen();
      socket.emitMessage('{"type":"hello","status":"running","writer":true}');
      socket.emitMessage(outputFrame(1n, 'history\x1b[5n'));
      socket.emitMessage('{"type":"replay_end","sequence":1}');
      terminal.emitData('\x1b[0n');
      terminal.emitBinary(String.fromCharCode(0x80, 0xff));
    });
    expect(socket.sent.filter((value) => value instanceof ArrayBuffer)).toHaveLength(0);
    expect(terminal.options.disableStdin).toBe(true);

    act(() => terminal.drainWrite());
    await waitFor(() =>
      expect(screen.getByText('测试会话').closest('.terminal-view')?.getAttribute('data-replay-complete')).toBe('true'),
    );
    expect(terminal.options.disableStdin).toBe(false);

    act(() => {
      terminal.emitData('live');
      terminal.emitBinary(String.fromCharCode(0x80, 0xff));
    });
    let frames = socket.sent.filter((value): value is ArrayBuffer => value instanceof ArrayBuffer);
    const [textFrame, binaryFrame] = frames;
    expect(textFrame).toBeDefined();
    expect(binaryFrame).toBeDefined();
    if (!textFrame || !binaryFrame) throw new Error('expected text and binary frames');
    expect(new TextDecoder().decode(new Uint8Array(textFrame).subarray(1))).toBe('live');
    expect([...new Uint8Array(binaryFrame)]).toEqual([0, 0x80, 0xff]);

    fireEvent.click(screen.getByRole('button', { name: '粘贴' }));
    await waitFor(() => expect(terminal.paste).toHaveBeenCalledWith(largePaste));
    expect(readText).toHaveBeenCalledOnce();
    frames = socket.sent.filter((value): value is ArrayBuffer => value instanceof ArrayBuffer).slice(2);
    expect(frames.length).toBeGreaterThan(1);
    expect(frames.every((frame) => frame.byteLength <= MAX_INPUT_FRAME_BYTES)).toBe(true);
    const decoder = new TextDecoder('utf-8', { fatal: true });
    expect(frames.map((frame) => decoder.decode(new Uint8Array(frame).subarray(1))).join('')).toBe(largePaste);
  });

  it('explains why clipboard buttons cannot work on an insecure HTTP origin', async () => {
    Object.defineProperty(window, 'isSecureContext', { configurable: true, value: false });
    const notify = vi.fn();
    renderTerminal(notify);
    await finishFontLoading();

    fireEvent.click(screen.getByRole('button', { name: '粘贴' }));
    expect(notify).toHaveBeenCalledWith(expect.stringContaining('不是安全上下文'), 'error');
    expect(navigator.clipboard.readText).not.toHaveBeenCalled();
  });

  it('uses the current xterm cursor mode and consumes mobile Ctrl/Alt modifiers once', async () => {
    renderTerminal();
    await finishFontLoading();
    const terminal = xtermHarness.FakeTerminal.instances[0];
    const socket = FakeWebSocket.instances[0];
    if (!terminal || !socket) throw new Error('terminal transport was not initialized');

    act(() => {
      socket.emitOpen();
      socket.emitMessage('{"type":"hello","status":"running","writer":true}');
    });

    terminal.modes.applicationCursorKeysMode = true;
    fireEvent.click(screen.getByRole('button', { name: 'Ctrl' }));
    act(() => terminal.emitData('\x1b[0n'));
    expect(screen.getByRole('button', { name: 'Ctrl' }).className).toBe('is-active');
    expect(socket.sent.filter((value) => value instanceof ArrayBuffer)).toHaveLength(0);

    act(() => socket.emitMessage('{"type":"replay_end","sequence":0}'));
    await waitFor(() =>
      expect(screen.getByText('测试会话').closest('.terminal-view')?.getAttribute('data-replay-complete')).toBe('true'),
    );

    fireEvent.click(screen.getByRole('button', { name: '向左' }));
    fireEvent.click(screen.getByRole('button', { name: '向上' }));
    fireEvent.click(screen.getByRole('button', { name: 'Alt' }));
    fireEvent.click(screen.getByRole('button', { name: '向右' }));

    const frames = socket.sent.filter((value): value is ArrayBuffer => value instanceof ArrayBuffer);
    const decoder = new TextDecoder();
    expect(frames.map((frame) => decoder.decode(new Uint8Array(frame).subarray(1)))).toEqual([
      '\x1b[1;5D',
      '\x1bOA',
      '\x1b[1;3C',
    ]);
  });

  it('asks the server to retry the backend before reattaching after a remote restart', async () => {
    renderTerminal();
    await finishFontLoading();
    const socket = FakeWebSocket.instances[0];
    if (!socket) throw new Error('terminal transport was not initialized');

    act(() => {
      socket.emitOpen();
      socket.emitMessage('{"type":"hello","status":"running","writer":true}');
      socket.emitMessage('{"type":"disconnect","status":"reconnecting","reason":"restarted"}');
    });
    expect(screen.getByText('会话已在其他设备重启，正在重新连接')).toBeTruthy();

    await act(async () => {
      socket.emitClose(1013);
      await Promise.resolve();
    });

    fireEvent.click(screen.getByRole('button', { name: '重试后台连接' }));
    await waitFor(() => expect(apiHarness.reconnectSession).toHaveBeenCalledWith('session-1'));
    await waitFor(() => expect(FakeWebSocket.instances).toHaveLength(2));
  });

  it('reports a failed backend retry without reattaching', async () => {
    apiHarness.reconnectSession.mockRejectedValue(new ApiError('无法重试', 502, { code: 'terminal_stop_failed' }));
    renderTerminal();
    await finishFontLoading();
    const socket = FakeWebSocket.instances[0];
    if (!socket) throw new Error('terminal transport was not initialized');

    await act(async () => {
      socket.emitOpen();
      socket.emitMessage('{"type":"hello","status":"running","writer":true}');
      socket.emitClose(1013);
      await Promise.resolve();
    });

    fireEvent.click(screen.getByRole('button', { name: '重试后台连接' }));
    await waitFor(() => expect(screen.getByText('无法结束会话，请稍后重试。')).toBeTruthy());
    expect(FakeWebSocket.instances).toHaveLength(1);
  });
});
