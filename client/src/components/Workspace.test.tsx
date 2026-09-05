// @vitest-environment jsdom

import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { Host, Session, User } from '../types';
import { Workspace } from './Workspace';

type SidebarStubProps = {
  sessions: Session[];
  restartingIds?: ReadonlySet<string> | undefined;
  onDelete: (session: Session) => void;
  onRestart: (session: Session) => void;
};

vi.mock('./Sidebar', () => ({
  Sidebar: ({ sessions, restartingIds = new Set<string>(), onDelete, onRestart }: SidebarStubProps) => (
    <>
      <button onClick={() => sessions[0] && onDelete(sessions[0])}>请求结束</button>
      <button
        disabled={Boolean(sessions[0] && restartingIds.has(sessions[0].id))}
        onClick={() => sessions[0] && onRestart(sessions[0])}
      >
        请求重启
      </button>
    </>
  ),
}));
vi.mock('./HostManager', () => ({ HostManager: () => null }));
vi.mock('./SessionDialog', () => ({ SessionDialog: () => null, RenameSessionDialog: () => null }));
vi.mock('./SettingsDialog', () => ({ SettingsDialog: () => null }));
vi.mock('./TerminalView', () => ({ TerminalView: () => null }));

const apiHarness = vi.hoisted(() => ({
  sessions: vi.fn(),
  hosts: vi.fn(),
  deleteSession: vi.fn(),
  restartSession: vi.fn(),
}));

vi.mock('../api', async (importOriginal) => ({
  ...(await importOriginal<typeof import('../api')>()),
  api: apiHarness,
}));

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

function renderWorkspace(sessions: Session[] = [session]) {
  return render(
    <Workspace
      initialHosts={[] satisfies Host[]}
      initialSessions={sessions}
      user={user}
      version="test"
      onLogout={vi.fn(async () => undefined)}
    />,
  );
}

beforeEach(() => {
  localStorage.clear();
  apiHarness.sessions.mockReset();
  apiHarness.hosts.mockReset();
  apiHarness.deleteSession.mockReset();
  apiHarness.restartSession.mockReset();
  apiHarness.sessions.mockResolvedValue([session]);
  apiHarness.hosts.mockResolvedValue([]);
  apiHarness.deleteSession.mockResolvedValue(undefined);
  apiHarness.restartSession.mockResolvedValue(session);
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
  it('names the session and its consequence in the confirmation dialog', () => {
    renderWorkspace();

    fireEvent.click(screen.getByRole('button', { name: '请求结束' }));

    expect(screen.getByRole('dialog', { name: '结束「构建任务」？' })).toBeTruthy();
    expect(screen.getByText('将结束进程并删除终端历史。')).toBeTruthy();
  });

  it('surfaces the server warning when the remote backend could not be confirmed stopped', async () => {
    apiHarness.deleteSession.mockResolvedValue({ warning: '无法连接主机，远端后台会话可能仍在运行' });
    renderWorkspace();

    fireEvent.click(screen.getByRole('button', { name: '请求结束' }));
    fireEvent.click(
      within(screen.getByRole('dialog', { name: '结束「构建任务」？' })).getByRole('button', {
        name: '结束会话',
      }),
    );

    await waitFor(() => expect(screen.getByText('无法连接主机，远端后台会话可能仍在运行')).toBeTruthy());
    expect(screen.queryByText('已结束并删除「构建任务」')).toBeNull();
  });
});

describe('Workspace restart safeguards', () => {
  it('confirms before restarting a live session and ignores repeat requests while one is in flight', async () => {
    let finishRestart!: (value: Session) => void;
    apiHarness.restartSession.mockImplementation(
      () =>
        new Promise<Session>((resolve) => {
          finishRestart = resolve;
        }),
    );
    renderWorkspace();

    fireEvent.click(screen.getByRole('button', { name: '请求重启' }));
    expect(apiHarness.restartSession).not.toHaveBeenCalled();
    const confirmation = screen.getByRole('dialog', { name: '重启「构建任务」？' });

    fireEvent.click(within(confirmation).getByRole('button', { name: '重启会话' }));
    expect(apiHarness.restartSession).toHaveBeenCalledOnce();

    fireEvent.click(screen.getByRole('button', { name: '请求重启' }));
    fireEvent.click(within(confirmation).getByRole('button', { name: '重启会话' }));
    expect(apiHarness.restartSession).toHaveBeenCalledOnce();

    await act(async () => finishRestart({ ...session, status: 'connecting' }));
    await waitFor(() => expect(screen.queryByRole('dialog', { name: '重启「构建任务」？' })).toBeNull());
  });

  it('restarts an already exited session without a confirmation step', async () => {
    renderWorkspace([{ ...session, status: 'exited' }]);

    fireEvent.click(screen.getByRole('button', { name: '请求重启' }));

    await waitFor(() => expect(apiHarness.restartSession).toHaveBeenCalledWith('session-1'));
    expect(screen.queryByRole('dialog', { name: '重启「构建任务」？' })).toBeNull();
  });
});

describe('Workspace session polling', () => {
  it('discards a slow poll answer that lost the race to a newer one', async () => {
    let resolveFirst!: (value: Session[]) => void;
    let resolveSecond!: (value: Session[]) => void;
    apiHarness.sessions
      .mockImplementationOnce(
        () =>
          new Promise<Session[]>((resolve) => {
            resolveFirst = resolve;
          }),
      )
      .mockImplementationOnce(
        () =>
          new Promise<Session[]>((resolve) => {
            resolveSecond = resolve;
          }),
      );
    renderWorkspace();

    document.dispatchEvent(new Event('visibilitychange'));
    document.dispatchEvent(new Event('visibilitychange'));

    await act(async () => {
      resolveSecond([{ ...session, name: '新结果' }]);
      resolveFirst([{ ...session, name: '旧结果' }]);
      await Promise.resolve();
    });

    await waitFor(() => expect(screen.getByText('新结果')).toBeTruthy());
    expect(screen.queryByText('旧结果')).toBeNull();
  });
});
