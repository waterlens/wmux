export type LiveStatus = 'connecting' | 'running' | 'reconnecting' | 'detached' | 'exited' | 'error' | 'offline';

export type ControlMessage = {
  type?: string;
  status?: string;
  reason?: string;
  writer?: boolean;
  message?: string;
  sequence?: number;
  truncated?: boolean;
};

export function websocketAddress(
  sessionId: string,
  sequence: number,
  location: Pick<Location, 'protocol' | 'host'> = window.location,
): string {
  const scheme = location.protocol === 'https:' ? 'wss:' : 'ws:';
  return `${scheme}//${location.host}/ws/sessions/${encodeURIComponent(sessionId)}?since=${sequence}`;
}

export function encodeTerminalInput(value: string): ArrayBuffer {
  const content = new TextEncoder().encode(value);
  const packet = new Uint8Array(content.length + 1);
  packet[0] = 0;
  packet.set(content, 1);
  return packet.buffer;
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
      ...(typeof record.truncated === 'boolean' ? { truncated: record.truncated } : {}),
    };
  } catch {
    return null;
  }
}
