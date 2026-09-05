import { describe, expect, it } from 'vitest';
import {
  applyTerminalModifiers,
  decodeTerminalOutput,
  encodeCursorKey,
  encodeTerminalBinaryFrames,
  encodeTerminalTextFrames,
  isPermanentSocketClose,
  MAX_INPUT_FRAME_BYTES,
  normalizeLiveStatus,
  parseControlMessage,
  ReplayBarrier,
  websocketAddress,
} from './terminalProtocol';

describe('terminal binary protocol', () => {
  it('encodes UTF-8 input with the client frame discriminator', () => {
    const [frame] = encodeTerminalTextFrames('你好');
    expect(frame).toBeDefined();
    if (!frame) throw new Error('missing encoded text frame');
    const packet = new Uint8Array(frame);
    expect(packet[0]).toBe(0);
    expect(new TextDecoder().decode(packet.slice(1))).toBe('你好');
  });

  it('chunks large text below 128 KiB without splitting UTF-8 code points', () => {
    const value = `${'a'.repeat(MAX_INPUT_FRAME_BYTES - 4)}😀你好`;
    const frames = encodeTerminalTextFrames(value);
    const decoder = new TextDecoder('utf-8', { fatal: true });

    expect(frames.length).toBeGreaterThan(1);
    expect(frames.every((frame) => frame.byteLength <= MAX_INPUT_FRAME_BYTES)).toBe(true);
    expect(frames.map((frame) => decoder.decode(new Uint8Array(frame).subarray(1))).join('')).toBe(value);
  });

  it('keeps onBinary code units as raw 8-bit bytes', () => {
    const binary = String.fromCharCode(0x00, 0x7f, 0x80, 0xff);
    const [frame] = encodeTerminalBinaryFrames(binary);
    expect(frame).toBeDefined();
    if (!frame) throw new Error('missing encoded binary frame');
    expect([...new Uint8Array(frame)]).toEqual([0, 0x00, 0x7f, 0x80, 0xff]);
  });

  it('decodes a big-endian output sequence and payload', () => {
    const packet = new Uint8Array(12);
    packet[0] = 1;
    new DataView(packet.buffer).setBigUint64(1, 513n, false);
    packet.set([0x61, 0x62, 0x63], 9);

    const frame = decodeTerminalOutput(packet.buffer);
    expect(frame?.sequence).toBe(513);
    expect(new TextDecoder().decode(frame?.output)).toBe('abc');
    expect(decodeTerminalOutput(new Uint8Array([2, 0]).buffer)).toBeNull();
  });

  it('applies one-shot Ctrl and Alt modifiers', () => {
    expect(applyTerminalModifiers('c', true, false)).toBe('\x03');
    expect(applyTerminalModifiers(' ', true, true)).toBe('\x1b\x00');
    expect(applyTerminalModifiers('hello', true, false)).toBe('hello');
  });

  it('uses DEC application cursor mode and xterm modifier parameters', () => {
    expect(encodeCursorKey('up', false, false, false)).toBe('\x1b[A');
    expect(encodeCursorKey('up', true, false, false)).toBe('\x1bOA');
    expect(encodeCursorKey('left', true, true, false)).toBe('\x1b[1;5D');
    expect(encodeCursorKey('right', false, false, true)).toBe('\x1b[1;3C');
    expect(encodeCursorKey('down', true, true, true)).toBe('\x1b[1;7B');
  });

  it('does not open replay input until every queued xterm write drains', () => {
    const barrier = new ReplayBarrier();
    barrier.reset();
    const first = barrier.trackReplayWrite();
    const second = barrier.trackReplayWrite();

    expect(barrier.endReplay()).toBe(false);
    expect(barrier.isOpen()).toBe(false);
    expect(first()).toBe(false);
    expect(barrier.isOpen()).toBe(false);
    expect(second()).toBe(true);
    expect(barrier.isOpen()).toBe(true);
  });

  it('ignores a delayed write callback from an older socket generation', () => {
    const barrier = new ReplayBarrier();
    barrier.reset();
    const staleWrite = barrier.trackReplayWrite();
    barrier.reset();
    const currentWrite = barrier.trackReplayWrite();
    expect(barrier.endReplay()).toBe(false);

    expect(staleWrite()).toBe(false);
    expect(barrier.isOpen()).toBe(false);
    expect(currentWrite()).toBe(true);
    expect(barrier.isOpen()).toBe(true);
  });
});

describe('terminal control protocol', () => {
  it('normalizes legacy lifecycle states', () => {
    expect(normalizeLiveStatus('disconnected')).toBe('reconnecting');
    expect(normalizeLiveStatus('terminated')).toBe('exited');
    expect(normalizeLiveStatus('unexpected')).toBeNull();
  });

  it('parses disconnect reasons and compatible writer fields', () => {
    expect(
      parseControlMessage('{"type":"disconnect","status":"reconnecting","reason":"server_shutdown","isWriter":false}'),
    ).toMatchObject({
      type: 'disconnect',
      status: 'reconnecting',
      reason: 'server_shutdown',
      writer: false,
    });
    expect(parseControlMessage('{"type":"writer","writable":true}')).toMatchObject({ writer: true });
    expect(parseControlMessage('{"type":"replay_end","sequence":42}')).toEqual({
      type: 'replay_end',
      sequence: 42,
    });
    expect(parseControlMessage('not json')).toBeNull();
  });

  it('keeps the execution generation and restart reason from lifecycle events', () => {
    expect(parseControlMessage('{"type":"hello","status":"running","generation":4,"writer":true}')).toMatchObject({
      type: 'hello',
      generation: 4,
      writer: true,
    });
    expect(parseControlMessage('{"type":"disconnect","status":"reconnecting","reason":"restarted"}')).toMatchObject({
      reason: 'restarted',
    });
    expect(parseControlMessage('{"type":"state","generation":"4"}')).toEqual({ type: 'state' });
  });

  it('builds an encoded WebSocket replay URL', () => {
    expect(websocketAddress('ssh/a b', 19, { protocol: 'https:', host: 'wmux.example' })).toBe(
      'wss://wmux.example/ws/sessions/ssh%2Fa%20b?since=19',
    );
  });

  it('only treats policy violations as permanent transport closes', () => {
    expect(isPermanentSocketClose(1008)).toBe(true);
    expect(isPermanentSocketClose(1012)).toBe(false);
    expect(isPermanentSocketClose(1013)).toBe(false);
  });
});
