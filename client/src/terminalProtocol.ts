export type LiveStatus = 'connecting' | 'running' | 'reconnecting' | 'detached' | 'exited' | 'error';

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

// keep in sync with internal/api/websocket.go
export const CLIENT_INPUT_FRAME = 0;
export const SERVER_OUTPUT_FRAME = 1;
export const OUTPUT_FRAME_HEADER_BYTES = 9;
export const MAX_INPUT_FRAME_BYTES = 128 * 1024;

const INPUT_FRAME_HEADER_BYTES = 1;
const MAX_INPUT_PAYLOAD_BYTES = MAX_INPUT_FRAME_BYTES - INPUT_FRAME_HEADER_BYTES;

export function websocketAddress(
  sessionId: string,
  sequence: number,
  location: Pick<Location, 'protocol' | 'host'> = window.location,
): string {
  const scheme = location.protocol === 'https:' ? 'wss:' : 'ws:';
  return `${scheme}//${location.host}/ws/sessions/${encodeURIComponent(sessionId)}?since=${sequence}`;
}

function inputPacket(content: Uint8Array): ArrayBuffer {
  const packet = new Uint8Array(new ArrayBuffer(content.length + INPUT_FRAME_HEADER_BYTES));
  packet[0] = CLIENT_INPUT_FRAME;
  packet.set(content, INPUT_FRAME_HEADER_BYTES);
  return packet.buffer;
}

/** Encode terminal text as independently valid UTF-8 WebSocket messages. */
export function encodeTerminalTextFrames(value: string): ArrayBuffer[] {
  if (!value) return [];

  const encoder = new TextEncoder();
  const frames: ArrayBuffer[] = [];
  let offset = 0;

  while (offset < value.length) {
    const payload = new Uint8Array(new ArrayBuffer(MAX_INPUT_PAYLOAD_BYTES));
    // encodeInto stops on a code point boundary, so no frame splits a UTF-8 scalar value.
    const { read, written } = encoder.encodeInto(value.slice(offset), payload);
    frames.push(inputPacket(payload.subarray(0, written)));
    offset += read;
  }

  return frames;
}

/** Preserve xterm's onBinary payload as 8-bit bytes rather than UTF-8 text. */
export function encodeTerminalBinaryFrames(value: string): ArrayBuffer[] {
  if (!value) return [];

  const frames: ArrayBuffer[] = [];
  for (let offset = 0; offset < value.length; offset += MAX_INPUT_PAYLOAD_BYTES) {
    const chunkLength = Math.min(MAX_INPUT_PAYLOAD_BYTES, value.length - offset);
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
  if (packet.length < OUTPUT_FRAME_HEADER_BYTES || packet[0] !== SERVER_OUTPUT_FRAME) return null;
  const sequence = Number(new DataView(data, 1, 8).getBigUint64(0, false));
  if (!Number.isSafeInteger(sequence)) return null;
  return { sequence, output: new Uint8Array(data.slice(OUTPUT_FRAME_HEADER_BYTES)) };
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

/** Holds terminal input closed until xterm has parsed every replay write. */
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
    return {
      ...(typeof record.type === 'string' ? { type: record.type } : {}),
      ...(typeof record.status === 'string' ? { status: record.status } : {}),
      ...(typeof record.reason === 'string' ? { reason: record.reason } : {}),
      ...(typeof record.writer === 'boolean' ? { writer: record.writer } : {}),
      ...(typeof record.message === 'string' ? { message: record.message } : {}),
      ...(typeof record.sequence === 'number' ? { sequence: record.sequence } : {}),
      ...(typeof record.generation === 'number' ? { generation: record.generation } : {}),
      ...(typeof record.truncated === 'boolean' ? { truncated: record.truncated } : {}),
    };
  } catch {
    return null;
  }
}
