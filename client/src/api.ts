import { z } from 'zod';
import type { HostInput, SessionInput } from './types';

export const AUTH_EXPIRED_EVENT = 'wmux:auth-expired';

const userSchema = z.object({
  username: z.string(),
  createdAt: z.string(),
});

const statusSchema = z.object({
  setupRequired: z.boolean(),
  authenticated: z.boolean(),
  version: z.string(),
  commit: z.string().optional(),
});

const hostSchema = z.object({
  id: z.string(),
  name: z.string(),
  address: z.string(),
  port: z.number().int(),
  username: z.string(),
  authType: z.enum(['password', 'privateKey', 'agent']),
  fingerprint: z.string().optional(),
  hasSecret: z.boolean(),
  createdAt: z.string(),
  updatedAt: z.string(),
});

const sshConfigCandidateSchema = z.object({
  alias: z.string(),
  address: z.string(),
  port: z.number().int().min(1).max(65_535),
  username: z.string(),
  hasIdentityFile: z.boolean(),
  unsupported: z.array(z.string()),
  existingHostId: z.string().optional(),
});

const sshConfigDiscoverySchema = z.object({
  available: z.boolean(),
  source: z.string(),
  candidates: z.array(sshConfigCandidateSchema),
});

const sessionSchema = z.object({
  id: z.string(),
  name: z.string(),
  kind: z.enum(['local', 'ssh']),
  hostId: z.string().optional(),
  hostName: z.string().optional(),
  cwd: z.string().optional(),
  command: z.string().optional(),
  persistence: z.enum(['auto', 'tmux', 'screen', 'none']),
  backend: z.string().optional(),
  backendName: z.string().optional(),
  status: z.enum(['connecting', 'running', 'reconnecting', 'detached', 'exited', 'error']),
  cols: z.number().int(),
  rows: z.number().int(),
  createdAt: z.string(),
  updatedAt: z.string(),
  lastAttachedAt: z.string().optional(),
  exitCode: z.number().int().optional(),
  error: z.string().optional(),
});

const probeSchema = z.object({
  fingerprint: z.string(),
  algorithm: z.string(),
});

const testResultSchema = z.object({
  ok: z.boolean(),
  latencyMs: z.number(),
});

const apiErrorSchema = z.object({
  error: z.object({
    code: z.string(),
    message: z.string(),
  }),
});

export class ApiError extends Error {
  readonly status: number;
  readonly code: string | undefined;
  readonly details: unknown;

  constructor(message: string, status: number, options: { code?: string | undefined; details?: unknown } = {}) {
    super(message);
    this.name = 'ApiError';
    this.status = status;
    this.code = options.code;
    this.details = options.details;
  }
}

export function signalAuthExpired(): void {
  if (typeof window !== 'undefined') window.dispatchEvent(new Event(AUTH_EXPIRED_EVENT));
}

async function request<T>(
  path: string,
  schema: z.ZodType<T>,
  init: RequestInit = {},
  redirectOnUnauthorized = true,
): Promise<T> {
  const headers = new Headers(init.headers);
  if (init.body !== undefined && !(init.body instanceof FormData)) headers.set('Content-Type', 'application/json');

  let response: Response;
  try {
    response = await fetch(path, { ...init, headers, credentials: 'same-origin' });
  } catch {
    throw new ApiError('无法连接到 wmux 服务。', 0);
  }

  const contentType = response.headers.get('content-type') ?? '';
  const payload: unknown =
    response.status === 204
      ? undefined
      : contentType.includes('json')
        ? await response.json().catch(() => undefined)
        : await response.text().catch(() => undefined);

  if (!response.ok) {
    const parsedError = apiErrorSchema.safeParse(payload);
    const code = parsedError.success ? parsedError.data.error.code : undefined;
    const message = parsedError.success ? parsedError.data.error.message : `请求失败 (${response.status})`;
    if (response.status === 401 && redirectOnUnauthorized) signalAuthExpired();
    throw new ApiError(message, response.status, { code, details: payload });
  }

  const parsed = schema.safeParse(payload);
  if (!parsed.success) {
    throw new ApiError('服务返回的数据格式不正确。', 502, { code: 'invalid_response', details: parsed.error });
  }
  return parsed.data;
}

function body(value: unknown): string {
  return JSON.stringify(value);
}

export const api = {
  status: () => request('/api/status', statusSchema),
  setup: (username: string, password: string) =>
    request('/api/setup', userSchema, { method: 'POST', body: body({ username, password }) }, false),
  login: (username: string, password: string) =>
    request('/api/login', userSchema, { method: 'POST', body: body({ username, password }) }, false),
  logout: () => request('/api/logout', z.undefined(), { method: 'POST' }),
  me: () => request('/api/me', userSchema),
  changePassword: (currentPassword: string, newPassword: string) =>
    request('/api/me/password', z.undefined(), { method: 'POST', body: body({ currentPassword, newPassword }) }),

  hosts: () => request('/api/hosts', z.array(hostSchema)),
  sshConfigHosts: () => request('/api/hosts/ssh-config', sshConfigDiscoverySchema),
  importSSHConfigHost: (alias: string) =>
    request('/api/hosts/import-ssh-config', hostSchema, { method: 'POST', body: body({ alias }) }),
  createHost: (input: HostInput) => request('/api/hosts', hostSchema, { method: 'POST', body: body(input) }),
  updateHost: (id: string, input: Partial<HostInput>) =>
    request(`/api/hosts/${encodeURIComponent(id)}`, hostSchema, { method: 'PATCH', body: body(input) }),
  deleteHost: (id: string) => request(`/api/hosts/${encodeURIComponent(id)}`, z.undefined(), { method: 'DELETE' }),
  probeHost: (id: string) => request(`/api/hosts/${encodeURIComponent(id)}/probe`, probeSchema, { method: 'POST' }),
  trustHost: (id: string, fingerprint: string) =>
    request(`/api/hosts/${encodeURIComponent(id)}/trust`, z.undefined(), {
      method: 'POST',
      body: body({ fingerprint }),
    }),
  testHost: (id: string) => request(`/api/hosts/${encodeURIComponent(id)}/test`, testResultSchema, { method: 'POST' }),

  sessions: () => request('/api/sessions', z.array(sessionSchema)),
  createSession: (input: SessionInput) =>
    request('/api/sessions', sessionSchema, { method: 'POST', body: body(input) }),
  updateSession: (id: string, input: { name?: string; cols?: number; rows?: number }) =>
    request(`/api/sessions/${encodeURIComponent(id)}`, sessionSchema, { method: 'PATCH', body: body(input) }),
  deleteSession: (id: string) =>
    request(`/api/sessions/${encodeURIComponent(id)}`, z.undefined(), { method: 'DELETE' }),
  restartSession: (id: string) =>
    request(`/api/sessions/${encodeURIComponent(id)}/restart`, sessionSchema, { method: 'POST' }),
};

const publicMessages: Partial<Record<string, string>> = {
  unauthorized: '登录已失效，请重新登录。',
  host_untrusted: '请先验证并信任 SSH 主机指纹。',
  host_in_use: '仍有会话使用这台主机，暂时无法删除。',
  fingerprint_changed: '主机指纹在确认期间发生变化，请重新验证。',
  host_exists: '这台 SSH 主机已经导入。',
  ssh_config_host_not_found: 'SSH config 中已找不到这台主机，请刷新后重试。',
  ssh_config_unsupported: '这台主机使用了 wmux 暂不支持的 SSH 代理配置。',
  ssh_config_invalid: 'SSH config 中的主机配置无效。',
  ssh_config_unavailable: '暂时无法读取 SSH config。',
  terminal_unavailable: '终端服务暂时不可用，请稍后重试。',
  ssh_probe_failed: '无法读取 SSH 主机指纹，请检查主机地址与网络连接。',
  ssh_test_failed: '无法连接到 SSH 主机，请检查地址、端口和认证信息。',
  terminal_start_failed: '无法启动会话，请检查工作目录、启动命令与主机连接。',
  terminal_stop_failed: '无法结束会话，请稍后重试。',
  terminal_config: '会话配置无效，请检查工作目录和启动命令。',
  internal_error: '服务暂时不可用，请稍后重试。',
};

export function errorMessage(error: unknown): string {
  if (error instanceof ApiError && error.code) {
    const message = publicMessages[error.code];
    if (message) return message;
  }
  return error instanceof Error ? error.message : '发生未知错误。';
}

export const schemas = {
  statusSchema,
  userSchema,
  hostSchema,
  sshConfigCandidateSchema,
  sshConfigDiscoverySchema,
  sessionSchema,
};
