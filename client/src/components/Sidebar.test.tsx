// @vitest-environment jsdom

import { cleanup, render, screen } from '@testing-library/react';
import { readFileSync } from 'node:fs';
import { afterEach, describe, expect, it, vi } from 'vitest';
import type { Host, Session, User } from '../types';
import { Sidebar } from './Sidebar';

const styles = readFileSync('client/src/styles.css', 'utf8');

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
    cols: 80,
    rows: 24,
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
    cols: 80,
    rows: 24,
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
      onDelete={callback}
      onSettings={callback}
      onCollapse={callback}
    />,
  );
}

describe('Sidebar information hierarchy', () => {
  it('omits aggregate counts and separates account controls from navigation', () => {
    const { container } = renderSidebar();

    expect(screen.queryByText('2')).toBeNull();
    expect(screen.queryByText('1')).toBeNull();
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

    const componentRule = styles.indexOf('.icon-button,');
    const hiddenMobileRule = styles.indexOf('.mobile-only {');
    expect(hiddenMobileRule).toBeGreaterThan(componentRule);
    const breakpoint = styles.slice(styles.indexOf('@media (max-width: 780px)'));
    expect(breakpoint).toMatch(/\.desktop-only\s*\{\s*display:\s*none;/);
    expect(breakpoint).toMatch(/\.mobile-only\s*\{\s*display:\s*inline-flex;/);
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
