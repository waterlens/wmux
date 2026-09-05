export type LiveStatus = 'connecting' | 'running' | 'reconnecting' | 'detached' | 'exited' | 'error' | 'offline';

export type ControlMessage = {
  type?: string;
  status?: string;
  reason?: string;
  writer?: boolean;
  message?: string;
  sequence?: number;
  generation?: number;
  truncated?: boolean;
};

export const MAX_INPUT_FRAME_BYTES = 128 * 1024;
const INPUT_FRAME_HEADER_BYTES = 1;

export function websocketAddress(
  sessionId: string,
  sequence: number,
  location: Pick<Location, 'protocol' | 'host'> = window.location,
): string {
  const scheme = location.protocol === 'https:' ? 'wss:' : 'ws:';
  return `${scheme}//${location.host}/ws/sessions/${encodeURIComponent(sessionId)}?since=${sequence}`;
}

function assertUsableFrameSize(maxFrameBytes: number): void {
  // Four payload bytes are required for the largest UTF-8 scalar value.
  if (!Number.isInteger(maxFrameBytes) || maxFrameBytes < INPUT_FRAME_HEADER_BYTES + 4) {
    throw new RangeError('maxFrameBytes must leave room for a complete UTF-8 code point');
  }
}

function inputPacket(content: Uint8Array): ArrayBuffer {
  const packet = new Uint8Array(new ArrayBuffer(content.length + INPUT_FRAME_HEADER_BYTES));
  packet[0] = 0;
  packet.set(content, INPUT_FRAME_HEADER_BYTES);
  return packet.buffer;
}

/** Encode terminal text as independently valid UTF-8 WebSocket messages. */
export function encodeTerminalTextFrames(value: string, maxFrameBytes = MAX_INPUT_FRAME_BYTES): ArrayBuffer[] {
  assertUsableFrameSize(maxFrameBytes);
  if (!value) return [];

  const encoder = new TextEncoder();
  const maxPayloadBytes = maxFrameBytes - INPUT_FRAME_HEADER_BYTES;
  const frames: ArrayBuffer[] = [];
  let offset = 0;

  while (offset < value.length) {
    const payload = new Uint8Array(new ArrayBuffer(maxPayloadBytes));
    const { read, written } = encoder.encodeInto(value.slice(offset), payload);
    if (!read) throw new Error('Unable to encode terminal input without splitting a UTF-8 code point');
    frames.push(inputPacket(payload.subarray(0, written)));
    offset += read;
  }

  return frames;
}

/**
 * Preserve xterm's onBinary payload as 8-bit bytes. TextEncoder must not be
 * used here: code units 0x80-0xff are already bytes, not Unicode text.
 */
export function encodeTerminalBinaryFrames(value: string, maxFrameBytes = MAX_INPUT_FRAME_BYTES): ArrayBuffer[] {
  assertUsableFrameSize(maxFrameBytes);
  if (!value) return [];

  const maxPayloadBytes = maxFrameBytes - INPUT_FRAME_HEADER_BYTES;
  const frames: ArrayBuffer[] = [];
  for (let offset = 0; offset < value.length; offset += maxPayloadBytes) {
    const chunkLength = Math.min(maxPayloadBytes, value.length - offset);
    const payload = new Uint8Array(new ArrayBuffer(chunkLength));
    for (let index = 0; index < chunkLength; index += 1) {
      payload[index] = value.charCodeAt(offset + index) & 0xff;
    }
    frames.push(inputPacket(payload));
  }
  return frames;
}

export function decodeTerminalOutput(data: ArrayBuffer): { sequence: number; output: Uint8Array<ArrayBuffer> } | null {
  const packet = new Uint8Array(data);
  if (packet.length < 9 || packet[0] !== 1) return null;
  const sequence = Number(new DataView(data, 1, 8).getBigUint64(0, false));
  if (!Number.isSafeInteger(sequence)) return null;
  return { sequence, output: new Uint8Array(data.slice(9)) };
}

export function applyTerminalModifiers(value: string, ctrl: boolean, alt: boolean): string {
  let output = value;
  if (ctrl && value.length === 1) {
    const code = value.toUpperCase().charCodeAt(0);
    if (code >= 64 && code <= 95) output = String.fromCharCode(code - 64);
    else if (value === ' ') output = '\x00';
    else if (value === '?') output = '\x7f';
  }
  return alt ? `\x1b${output}` : output;
}

export type CursorDirection = 'up' | 'down' | 'left' | 'right';

/** Encode a virtual cursor key using xterm/DEC cursor and modifier semantics. */
export function encodeCursorKey(
  direction: CursorDirection,
  applicationCursorMode: boolean,
  ctrl: boolean,
  alt: boolean,
): string {
  const final = { up: 'A', down: 'B', right: 'C', left: 'D' }[direction];
  if (ctrl || alt) {
    // xterm modifier parameter: 1 + Alt(2) + Ctrl(4).
    const modifier = 1 + (alt ? 2 : 0) + (ctrl ? 4 : 0);
    return `\x1b[1;${modifier}${final}`;
  }
  return applicationCursorMode ? `\x1bO${final}` : `\x1b[${final}`;
}

/**
 * Holds terminal input closed until xterm has parsed every replay write.
 * `replay_end` alone is not sufficient because Terminal.write is queued and
 * historical device-status queries can emit replies from a later microtask.
 */
export class ReplayBarrier {
  private generation = 0;
  private pendingWrites = 0;
  private replayEnded = false;
  private open = false;

  reset(): void {
    this.generation += 1;
    this.pendingWrites = 0;
    this.replayEnded = false;
    this.open = false;
  }

  isCollectingReplay(): boolean {
    return !this.replayEnded;
  }

  isOpen(): boolean {
    return this.open;
  }

  trackReplayWrite(): () => boolean {
    if (this.replayEnded) return () => false;
    const generation = this.generation;
    let completed = false;
    this.pendingWrites += 1;
    return () => {
      if (completed || generation !== this.generation) return false;
      completed = true;
      this.pendingWrites -= 1;
      return this.tryOpen();
    };
  }

  endReplay(): boolean {
    this.replayEnded = true;
    return this.tryOpen();
  }

  private tryOpen(): boolean {
    if (this.open || !this.replayEnded || this.pendingWrites !== 0) return false;
    this.open = true;
    return true;
  }
}

export function normalizeLiveStatus(value: unknown): LiveStatus | null {
  if (
    value === 'connecting' ||
    value === 'running' ||
    value === 'reconnecting' ||
    value === 'detached' ||
    value === 'exited' ||
    value === 'error'
  )
    return value;
  if (value === 'disconnected') return 'reconnecting';
  if (value === 'terminated' || value === 'stopped') return 'exited';
  return null;
}

export function isPermanentSocketClose(code: number): boolean {
  return code === 1008;
}

export function parseControlMessage(value: string): ControlMessage | null {
  try {
    const candidate = JSON.parse(value) as unknown;
    if (!candidate || typeof candidate !== 'object' || Array.isArray(candidate)) return null;
    const record = candidate as Record<string, unknown>;
    const writer =
      typeof record.writer === 'boolean'
        ? record.writer
        : typeof record.isWriter === 'boolean'
          ? record.isWriter
          : typeof record.writable === 'boolean'
            ? record.writable
            : undefined;
    return {
      ...(typeof record.type === 'string' ? { type: record.type } : {}),
      ...(typeof record.status === 'string' ? { status: record.status } : {}),
      ...(typeof record.reason === 'string' ? { reason: record.reason } : {}),
      ...(typeof writer === 'boolean' ? { writer } : {}),
      ...(typeof record.message === 'string' ? { message: record.message } : {}),
      ...(typeof record.sequence === 'number' ? { sequence: record.sequence } : {}),
      ...(typeof record.generation === 'number' ? { generation: record.generation } : {}),
      ...(typeof record.truncated === 'boolean' ? { truncated: record.truncated } : {}),
    };
  } catch {
    return null;
  }
}
