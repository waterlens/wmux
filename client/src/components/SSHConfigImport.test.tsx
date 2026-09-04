// @vitest-environment jsdom

import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { api } from '../api';
import type { Host } from '../types';
import { SSHConfigImport } from './SSHConfigImport';

const importedHost: Host = {
  id: 'host_imported',
  name: 'workbox',
  address: 'workbox.internal',
  port: 2200,
  username: 'dev',
  authType: 'agent',
  hasSecret: false,
  createdAt: '2026-09-04T00:00:00Z',
  updatedAt: '2026-09-04T00:00:00Z',
};

beforeEach(() => {
  document.body.innerHTML = '<div id="root"></div>';
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

describe('SSHConfigImport', () => {
  it('discovers on mount and imports only after an explicit click', async () => {
    vi.spyOn(api, 'sshConfigHosts').mockResolvedValue({
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
    });
    const importHost = vi.spyOn(api, 'importSSHConfigHost').mockResolvedValue(importedHost);
    const probeHost = vi.spyOn(api, 'probeHost');
    const trustHost = vi.spyOn(api, 'trustHost');
    const onImported = vi.fn();
    const notify = vi.fn();
    render(<SSHConfigImport onImported={onImported} notify={notify} />);

    await waitFor(() => expect(api.sshConfigHosts).toHaveBeenCalledOnce());
    expect(importHost).not.toHaveBeenCalled();
    fireEvent.click(screen.getByRole('button', { name: '从 SSH config 导入主机' }));
    expect(await screen.findByText('dev@workbox.internal:2200')).toBeTruthy();
    expect(screen.getByText('IdentityFile 不会导入')).toBeTruthy();
    expect(screen.getByRole('dialog').textContent).toContain('不会读取或复制私钥');

    fireEvent.click(screen.getByRole('button', { name: '导入 workbox' }));
    await waitFor(() => expect(importHost).toHaveBeenCalledWith('workbox'));
    expect(onImported).toHaveBeenCalledWith(importedHost);
    expect(probeHost).not.toHaveBeenCalled();
    expect(trustHost).not.toHaveBeenCalled();
    expect((screen.getByRole('button', { name: 'workbox 已导入' }) as HTMLButtonElement).disabled).toBe(true);
  });

  it('labels proxy directives and does not allow importing them', async () => {
    vi.spyOn(api, 'sshConfigHosts').mockResolvedValue({
      available: true,
      source: '~/.ssh/config',
      candidates: [
        {
          alias: 'jumpbox-target',
          address: 'target.internal',
          port: 22,
          username: 'dev',
          hasIdentityFile: false,
          unsupported: ['ProxyJump'],
        },
      ],
    });
    const importHost = vi.spyOn(api, 'importSSHConfigHost');
    render(<SSHConfigImport onImported={vi.fn()} notify={vi.fn()} />);

    fireEvent.click(screen.getByRole('button', { name: '从 SSH config 导入主机' }));
    expect(await screen.findByText('暂不支持 ProxyJump')).toBeTruthy();
    const button = screen.getByRole('button', { name: '导入 jumpbox-target' });
    expect(button.hasAttribute('disabled')).toBe(true);
    fireEvent.click(button);
    expect(importHost).not.toHaveBeenCalled();
  });
});
