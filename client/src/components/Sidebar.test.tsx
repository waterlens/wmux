// @vitest-environment jsdom

import { cleanup, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import type { Host, Session, User } from '../types';
import { Sidebar } from './Sidebar';

const user: User = { username: 'waterlens', createdAt: '2026-01-01T00:00:00Z' };
const hosts: Host[] = [
  {
    id: 'host-1',
    name: '工作站',
    address: 'host.example',
    port: 22,
    username: 'dev',
    authType: 'agent',
    fingerprint: 'SHA256:test',
    hasSecret: false,
    createdAt: '2026-01-01T00:00:00Z',
    updatedAt: '2026-01-01T00:00:00Z',
  },
];
const sessions: Session[] = [
  {
    id: 'local-session',
    name: '本机工作',
    kind: 'local',
    persistence: 'auto',
    status: 'running',
    createdAt: '2026-01-01T00:00:00Z',
    updatedAt: '2026-01-01T00:00:00Z',
  },
  {
    id: 'ssh-session',
    name: '远程工作',
    kind: 'ssh',
    hostId: 'host-1',
    hostName: '工作站',
    persistence: 'tmux',
    status: 'running',
    createdAt: '2026-01-01T00:00:00Z',
    updatedAt: '2026-01-01T00:00:00Z',
  },
];

afterEach(cleanup);

function renderSidebar() {
  const callback = vi.fn();
  return render(
    <Sidebar
      sessions={sessions}
      hosts={hosts}
      user={user}
      activeSessionId={null}
      currentView="home"
      mobileOpen={false}
      onMobileClose={callback}
      onSelectSession={callback}
      onNewSession={callback}
      onHome={callback}
      onHosts={callback}
      onRename={callback}
      onRestart={callback}
      restartingIds={new Set()}
      onDelete={callback}
      onSettings={callback}
      onCollapse={callback}
    />,
  );
}

describe('Sidebar information hierarchy', () => {
  it('separates account controls from navigation', () => {
    const { container } = renderSidebar();

    expect(container.querySelectorAll('.sidebar-section-title span')).toHaveLength(1);
    expect(container.querySelectorAll('.session-group__header em')).toHaveLength(0);
    expect(container.querySelectorAll('.sidebar-nav > em')).toHaveLength(0);

    const account = container.querySelector('.sidebar-account');
    const accountButton = screen.getByRole('button', { name: /waterlens.*管理员/ });
    expect(account).not.toBeNull();
    expect(account?.contains(accountButton)).toBe(true);
    expect(container.querySelector('.sidebar-footer-nav')?.contains(accountButton)).toBe(false);
  });

  it('assigns mutually exclusive desktop/mobile visibility utilities to header controls', () => {
    renderSidebar();
    expect(screen.getByRole('button', { name: '收起侧栏' }).classList.contains('desktop-only')).toBe(true);
    expect(screen.getByRole('button', { name: '关闭侧栏' }).classList.contains('mobile-only')).toBe(true);
  });

  it('exposes session groups as accessible disclosures', () => {
    const { container } = renderSidebar();
    const localGroup = screen.getByRole('button', { name: '本机' });
    const controlledID = localGroup.getAttribute('aria-controls');

    expect(localGroup.getAttribute('aria-expanded')).toBe('true');
    expect(controlledID).toBeTruthy();
    expect(container.querySelector(`[id="${controlledID}"]`)).not.toBeNull();
  });
});
