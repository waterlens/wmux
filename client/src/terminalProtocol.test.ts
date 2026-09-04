import { describe, expect, it } from 'vitest';
import {
  applyTerminalModifiers,
  decodeTerminalOutput,
  encodeTerminalInput,
  isPermanentSocketClose,
  normalizeLiveStatus,
  parseControlMessage,
  websocketAddress,
} from './terminalProtocol';

describe('terminal binary protocol', () => {
  it('encodes UTF-8 input with the client frame discriminator', () => {
    const packet = new Uint8Array(encodeTerminalInput('你好'));
    expect(packet[0]).toBe(0);
    expect(new TextDecoder().decode(packet.slice(1))).toBe('你好');
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
    expect(parseControlMessage('not json')).toBeNull();
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
