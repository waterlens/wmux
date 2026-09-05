import { afterEach, describe, expect, it, vi } from 'vitest';
import { ApiError, api, AUTH_EXPIRED_EVENT, schemas } from './api';

const sessionFixture = {
  id: 'session-1',
  name: '构建任务',
  kind: 'local',
  persistence: 'auto',
  status: 'running',
  cols: 80,
  rows: 24,
  createdAt: '2026-01-01T00:00:00Z',
  updatedAt: '2026-01-01T00:00:00Z',
};

const originalFetch = globalThis.fetch;

afterEach(() => {
  globalThis.fetch = originalFetch;
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

describe('API error handling', () => {
  it('surfaces the message from the unified nested error response', async () => {
    globalThis.fetch = vi.fn(
      async () =>
        new Response(JSON.stringify({ error: { code: 'host_untrusted', message: '请先验证并信任 SSH 主机密钥' } }), {
          status: 409,
          headers: { 'Content-Type': 'application/json' },
        }),
    );

    const request = api.status();

    await expect(request).rejects.toBeInstanceOf(ApiError);
    await expect(request).rejects.toMatchObject({
      status: 409,
      message: '请先验证并信任 SSH 主机密钥',
    });
  });

  it('rejects a successful response that violates the API contract', async () => {
    globalThis.fetch = vi.fn(
      async () =>
        new Response(JSON.stringify({ setupRequired: false, authenticated: true, version: 42 }), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        }),
    );

    await expect(api.status()).rejects.toMatchObject({
      status: 502,
      code: 'invalid_response',
      message: '服务返回的数据格式不正确。',
    });
  });

  it('keeps a rejected current password out of the global auth-expiry signal', async () => {
    const eventTarget = new EventTarget();
    const expired = vi.fn();
    eventTarget.addEventListener(AUTH_EXPIRED_EVENT, expired);
    vi.stubGlobal('window', eventTarget);
    globalThis.fetch = vi.fn(
      async () =>
        new Response(JSON.stringify({ error: { code: 'unauthorized', message: '当前密码不正确' } }), {
          status: 401,
          headers: { 'Content-Type': 'application/json' },
        }),
    );

    await expect(api.changePassword('wrong', 'new-password')).rejects.toMatchObject({ status: 401 });
    expect(expired).not.toHaveBeenCalled();
  });

  it('announces global authentication expiry on protected 401 responses', async () => {
    const eventTarget = new EventTarget();
    const expired = vi.fn();
    eventTarget.addEventListener(AUTH_EXPIRED_EVENT, expired);
    vi.stubGlobal('window', eventTarget);
    globalThis.fetch = vi.fn(
      async () =>
        new Response(JSON.stringify({ error: { code: 'unauthorized', message: '请重新登录' } }), {
          status: 401,
          headers: { 'Content-Type': 'application/json' },
        }),
    );

    await expect(api.sessions()).rejects.toMatchObject({ status: 401 });
    expect(expired).toHaveBeenCalledOnce();
  });
});

describe('SSH config API contract', () => {
  it('validates discovered hosts and imports one alias explicitly', async () => {
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce(
        new Response(
          JSON.stringify({
            available: true,
            source: '~/.ssh/config',
            candidates: [
              {
                alias: 'workbox',
                address: 'workbox.internal',
                port: 2200,
                username: 'dev',
                hasIdentityFile: true,
                unsupported: [],
              },
            ],
          }),
          { status: 200, headers: { 'Content-Type': 'application/json' } },
        ),
      )
      .mockResolvedValueOnce(
        new Response(
          JSON.stringify({
            id: 'host_imported',
            name: 'workbox',
            address: 'workbox.internal',
            port: 2200,
            username: 'dev',
            authType: 'agent',
            hasSecret: false,
            createdAt: '2026-09-04T00:00:00Z',
            updatedAt: '2026-09-04T00:00:00Z',
          }),
          { status: 201, headers: { 'Content-Type': 'application/json' } },
        ),
      );
    globalThis.fetch = fetchMock;

    await expect(api.sshConfigHosts()).resolves.toMatchObject({
      available: true,
      candidates: [{ alias: 'workbox', hasIdentityFile: true }],
    });
    await expect(api.importSSHConfigHost('workbox')).resolves.toMatchObject({
      id: 'host_imported',
      authType: 'agent',
      hasSecret: false,
    });
    expect(fetchMock).toHaveBeenNthCalledWith(
      2,
      '/api/hosts/import-ssh-config',
      expect.objectContaining({ method: 'POST', body: JSON.stringify({ alias: 'workbox' }) }),
    );
  });

  it('rejects malformed discovery metadata', async () => {
    globalThis.fetch = vi.fn(
      async () =>
        new Response(
          JSON.stringify({
            available: true,
            source: '~/.ssh/config',
            candidates: [{ alias: 'broken', address: 'host', port: 70_000, username: 'dev' }],
          }),
          { status: 200, headers: { 'Content-Type': 'application/json' } },
        ),
    );

    await expect(api.sshConfigHosts()).rejects.toMatchObject({ status: 502, code: 'invalid_response' });
  });
});

describe('session lifecycle API contract', () => {
  it('reads the delete warning when the backend could not be confirmed stopped', async () => {
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce(new Response(null, { status: 204 }))
      .mockResolvedValueOnce(
        new Response(JSON.stringify({ warning: '无法连接主机，远端后台会话可能仍在运行' }), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        }),
      );
    globalThis.fetch = fetchMock;

    await expect(api.deleteSession('session-1')).resolves.toBeUndefined();
    await expect(api.deleteSession('session-2')).resolves.toEqual({
      warning: '无法连接主机，远端后台会话可能仍在运行',
    });
    expect(fetchMock).toHaveBeenNthCalledWith(
      1,
      '/api/sessions/session-1',
      expect.objectContaining({ method: 'DELETE' }),
    );
  });

  it('posts the reconnect request and accepts an empty 204 body', async () => {
    const fetchMock = vi.fn(async () => new Response(null, { status: 204 }));
    globalThis.fetch = fetchMock;

    await expect(api.reconnectSession('session 1')).resolves.toBeUndefined();
    expect(fetchMock).toHaveBeenCalledWith(
      '/api/sessions/session%201/reconnect',
      expect.objectContaining({ method: 'POST' }),
    );
  });

  it('accepts a session generation and rejects a fractional one', () => {
    expect(schemas.sessionSchema.safeParse({ ...sessionFixture, generation: 3 }).success).toBe(true);
    expect(schemas.sessionSchema.safeParse(sessionFixture).success).toBe(true);
    expect(schemas.sessionSchema.safeParse({ ...sessionFixture, generation: 1.5 }).success).toBe(false);
  });
});
