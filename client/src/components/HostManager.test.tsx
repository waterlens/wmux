// @vitest-environment jsdom

import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { Host } from '../types';
import { HostManager } from './HostManager';

const apiHarness = vi.hoisted(() => ({
  createHost: vi.fn(),
  updateHost: vi.fn(),
  deleteHost: vi.fn(),
  probeHost: vi.fn(),
  trustHost: vi.fn(),
  testHost: vi.fn(),
  sshConfigHosts: vi.fn(),
  importSSHConfigHost: vi.fn(),
}));

vi.mock('../api', () => ({
  api: apiHarness,
  errorMessage: (reason: unknown) => (reason instanceof Error ? reason.message : '请求失败'),
}));

const host: Host = {
  id: 'host-1',
  name: '家庭服务器',
  address: 'server.example.com',
  port: 22,
  username: 'dev',
  authType: 'privateKey',
  hasSecret: true,
  createdAt: '2026-01-01T00:00:00Z',
  updatedAt: '2026-01-01T00:00:00Z',
};

beforeEach(() => {
  Object.values(apiHarness).forEach((mock) => mock.mockReset());
  apiHarness.sshConfigHosts.mockResolvedValue({
    available: false,
    source: '~/.ssh/config',
    candidates: [],
  });
});

afterEach(cleanup);

function renderManager(hosts: Host[]) {
  return render(<HostManager hosts={hosts} onHostsChange={vi.fn()} onStartSession={vi.fn()} notify={vi.fn()} />);
}

describe('HostManager copy and identity verification', () => {
  it('exposes and updates the selected authentication method', () => {
    renderManager([]);
    fireEvent.click(screen.getByRole('button', { name: /添加主机/ }));

    const group = screen.getByRole('group', { name: '认证方式' });
    const privateKey = within(group).getByRole('button', { name: 'SSH 私钥' });
    const password = within(group).getByRole('button', { name: '密码' });
    const agent = within(group).getByRole('button', { name: 'SSH 代理' });
    expect(privateKey.getAttribute('aria-pressed')).toBe('true');
    expect(password.getAttribute('aria-pressed')).toBe('false');
    expect(agent.getAttribute('aria-pressed')).toBe('false');

    fireEvent.click(agent);
    expect(privateKey.getAttribute('aria-pressed')).toBe('false');
    expect(password.getAttribute('aria-pressed')).toBe('false');
    expect(agent.getAttribute('aria-pressed')).toBe('true');
  });

  it('keeps create/edit chrome concise while preserving field validation', () => {
    renderManager([]);

    expect(screen.queryByText('主机密钥校验始终开启')).toBeNull();
    expect(screen.queryByText(/不会静默接受新指纹/)).toBeNull();

    fireEvent.click(screen.getByRole('button', { name: /添加主机/ }));
    expect(screen.getByRole('dialog', { name: '添加 SSH 主机' })).toBeTruthy();
    expect(screen.queryByText('凭据由 wmux 服务加密保管。')).toBeNull();
    expect(screen.queryByText('支持 OpenSSH PEM 格式')).toBeNull();
    expect(screen.queryByText(/保存后还需要探测并确认主机指纹/)).toBeNull();

    fireEvent.click(screen.getByRole('button', { name: '保存主机' }));
    expect(screen.getByRole('alert').textContent).toBe('请填写名称、地址和用户名');
  });

  it('still exposes the explicit fingerprint confirmation flow', async () => {
    apiHarness.probeHost.mockResolvedValue({
      fingerprint: 'SHA256:reviewed-fingerprint',
      algorithm: 'ssh-ed25519',
    });
    renderManager([host]);

    fireEvent.click(screen.getByRole('button', { name: '验证身份' }));

    await waitFor(() => expect(screen.getByRole('dialog', { name: '验证主机身份' })).toBeTruthy());
    expect(screen.getByText('ssh-ed25519')).toBeTruthy();
    expect(screen.getByText('SHA256:reviewed-fingerprint')).toBeTruthy();
    expect(screen.getByRole('button', { name: /信任此指纹/ })).toBeTruthy();
  });
});
