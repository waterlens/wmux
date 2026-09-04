// @vitest-environment jsdom

import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { Host, Session, User } from '../types';
import { Workspace } from './Workspace';

type SidebarStubProps = {
  sessions: Session[];
  onDelete: (session: Session) => void;
};

vi.mock('./Sidebar', () => ({
  Sidebar: ({ sessions, onDelete }: SidebarStubProps) => (
    <button onClick={() => sessions[0] && onDelete(sessions[0])}>请求结束</button>
  ),
}));
vi.mock('./HostManager', () => ({ HostManager: () => null }));
vi.mock('./SessionDialog', () => ({ SessionDialog: () => null, RenameSessionDialog: () => null }));
vi.mock('./SettingsDialog', () => ({ SettingsDialog: () => null }));
vi.mock('./TerminalView', () => ({ TerminalView: () => null }));

const user: User = { username: 'waterlens', createdAt: '2026-01-01T00:00:00Z' };
const session: Session = {
  id: 'session-1',
  name: '构建任务',
  kind: 'local',
  cwd: '~',
  persistence: 'auto',
  status: 'running',
  cols: 80,
  rows: 24,
  createdAt: '2026-01-01T00:00:00Z',
  updatedAt: '2026-01-01T00:00:00Z',
};

beforeEach(() => {
  localStorage.clear();
  vi.stubGlobal('matchMedia', (query: string) => ({
    matches: false,
    media: query,
    onchange: null,
    addListener: vi.fn(),
    removeListener: vi.fn(),
    addEventListener: vi.fn(),
    removeEventListener: vi.fn(),
    dispatchEvent: vi.fn(),
  }));
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe('Workspace termination confirmation', () => {
  it('states the single concrete consequence without generic warning copy', () => {
    render(
      <Workspace
        initialHosts={[] satisfies Host[]}
        initialSessions={[session]}
        user={user}
        version="test"
        onLogout={vi.fn(async () => undefined)}
      />,
    );

    fireEvent.click(screen.getByRole('button', { name: '请求结束' }));

    expect(screen.getByRole('dialog', { name: '结束「构建任务」？' })).toBeTruthy();
    expect(screen.getByText('将结束进程并删除终端历史。')).toBeTruthy();
    expect(screen.queryByText('这个操作无法撤销。')).toBeNull();
    expect(screen.queryByText(/若只想隐藏终端/)).toBeNull();
  });
});
