// @vitest-environment jsdom

import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { ApiError } from '../api';
import type { TerminalPreferences, User } from '../types';
import { SettingsDialog } from './SettingsDialog';

const apiHarness = vi.hoisted(() => ({ changePassword: vi.fn() }));

vi.mock('../api', async (importOriginal) => ({
  ...(await importOriginal<typeof import('../api')>()),
  api: apiHarness,
}));

const user: User = { username: 'waterlens', createdAt: '2026-01-01T00:00:00Z' };
const preferences: TerminalPreferences = {
  fontSize: 14,
  cursorStyle: 'block',
  cursorBlink: true,
  scrollback: 10_000,
  theme: 'light',
};

beforeEach(() => {
  apiHarness.changePassword.mockReset();
});
afterEach(cleanup);

function renderSettings(onLogout: () => Promise<void> = vi.fn(async (): Promise<void> => undefined)) {
  const onPreferencesChange = vi.fn();
  const onClose = vi.fn();
  return {
    onLogout,
    onClose,
    onPreferencesChange,
    ...render(
      <SettingsDialog
        user={user}
        version="1.0.0"
        commit="0123456789ab"
        hosts={[]}
        sessions={[]}
        preferences={preferences}
        onPreferencesChange={onPreferencesChange}
        onClose={onClose}
        onLogout={onLogout}
      />,
    ),
  };
}

function openSecuritySection() {
  fireEvent.click(screen.getByRole('button', { name: '账户与安全' }));
}

describe('SettingsDialog behavior', () => {
  it('labels terminal controls and forwards preference changes', () => {
    const { onPreferencesChange } = renderSettings();

    expect(screen.getByText('仅保存在此浏览器').classList.contains('field__hint')).toBe(true);
    expect(screen.getByRole('slider', { name: '终端字体大小' })).toBeTruthy();
    expect(screen.getByRole('combobox', { name: '光标样式' })).toBeTruthy();
    expect(screen.getByRole('combobox', { name: '回滚缓冲区' })).toBeTruthy();

    fireEvent.change(screen.getByRole('combobox', { name: '界面主题' }), { target: { value: 'dark' } });
    expect(onPreferencesChange).toHaveBeenLastCalledWith({ ...preferences, theme: 'dark' });

    fireEvent.click(screen.getByRole('checkbox', { name: '光标闪烁' }));
    expect(onPreferencesChange).toHaveBeenLastCalledWith({ ...preferences, cursorBlink: false });
  });

  it('keeps minimum-length validation and submits a valid password change', async () => {
    const browser = userEvent.setup();
    apiHarness.changePassword.mockResolvedValue(undefined);
    renderSettings();
    openSecuritySection();

    const currentPassword = screen.getByLabelText('当前密码');
    const newPassword = screen.getByLabelText(/^新密码/);
    const passwordConfirm = screen.getByLabelText('确认新密码');

    await browser.type(currentPassword, 'old-password');
    await browser.type(newPassword, 'short');
    await browser.type(passwordConfirm, 'short');
    await browser.click(screen.getByRole('button', { name: '更新密码' }));
    expect(screen.getByRole('alert').textContent).toBe('新密码至少需要 10 个字符');
    expect(apiHarness.changePassword).not.toHaveBeenCalled();

    await browser.clear(newPassword);
    await browser.clear(passwordConfirm);
    await browser.type(newPassword, 'new-password');
    await browser.type(passwordConfirm, 'new-password');
    await browser.click(screen.getByRole('button', { name: '更新密码' }));

    await waitFor(() => expect(apiHarness.changePassword).toHaveBeenCalledWith('old-password', 'new-password'));
    expect(screen.getByRole('status').textContent).toContain('密码已更新');
    expect((currentPassword as HTMLInputElement).value).toBe('');
    expect((newPassword as HTMLInputElement).value).toBe('');
    expect((passwordConfirm as HTMLInputElement).value).toBe('');
  });

  it('keeps settings open until a password change finishes', async () => {
    let finishPasswordChange!: () => void;
    apiHarness.changePassword.mockImplementation(
      () =>
        new Promise<void>((resolve) => {
          finishPasswordChange = resolve;
        }),
    );
    const { onClose } = renderSettings();
    openSecuritySection();
    fireEvent.change(screen.getByLabelText('当前密码'), { target: { value: 'old-password' } });
    fireEvent.change(screen.getByLabelText(/^新密码/), { target: { value: 'new-password' } });
    fireEvent.change(screen.getByLabelText('确认新密码'), { target: { value: 'new-password' } });
    fireEvent.click(screen.getByRole('button', { name: '更新密码' }));

    const close = screen.getByRole('button', { name: '关闭' });
    expect(close.hasAttribute('disabled')).toBe(true);
    expect(screen.getByRole('button', { name: '退出登录' }).hasAttribute('disabled')).toBe(true);
    fireEvent.click(close);
    fireEvent.keyDown(document, { key: 'Escape' });
    fireEvent.mouseDown(screen.getByRole('dialog', { name: '设置' }).parentElement!);
    expect(onClose).not.toHaveBeenCalled();

    await act(async () => finishPasswordChange());
    expect(screen.getByRole('status').textContent).toContain('密码已更新');
    expect(close.hasAttribute('disabled')).toBe(false);
    expect(screen.getByRole('button', { name: '退出登录' }).hasAttribute('disabled')).toBe(false);
    fireEvent.click(close);
    expect(onClose).toHaveBeenCalledOnce();
  });

  it('requires confirmation before logout and exposes progress', async () => {
    let finishLogout!: () => void;
    const onLogout = vi.fn(
      () =>
        new Promise<void>((resolve) => {
          finishLogout = resolve;
        }),
    );
    renderSettings(onLogout);
    openSecuritySection();

    fireEvent.click(screen.getByRole('button', { name: '退出登录' }));
    const confirmation = screen.getByRole('dialog', { name: '退出 wmux？' });
    expect(onLogout).not.toHaveBeenCalled();
    expect(within(confirmation).getByText(/持久化会话仍会在后台继续运行/)).toBeTruthy();

    const confirmButton = within(confirmation).getByRole('button', { name: '退出登录' });
    fireEvent.click(confirmButton);
    expect(onLogout).toHaveBeenCalledOnce();
    expect(confirmButton.getAttribute('aria-busy')).toBe('true');
    expect((confirmButton as HTMLButtonElement).disabled).toBe(true);

    await act(async () => finishLogout());
    await waitFor(() => expect(screen.queryByRole('dialog', { name: '退出 wmux？' })).toBeNull());
  });
  it('reports a rejected current password without ending the session', async () => {
    apiHarness.changePassword.mockRejectedValue(new ApiError('未授权', 401, { code: 'unauthorized' }));
    renderSettings();
    openSecuritySection();

    fireEvent.change(screen.getByLabelText('当前密码'), { target: { value: 'wrong-password' } });
    fireEvent.change(screen.getByLabelText(/^新密码/), { target: { value: 'new-password' } });
    fireEvent.change(screen.getByLabelText('确认新密码'), { target: { value: 'new-password' } });
    fireEvent.click(screen.getByRole('button', { name: '更新密码' }));

    await waitFor(() => expect(screen.getByRole('alert').textContent).toBe('当前密码不正确'));
    expect((screen.getByLabelText('当前密码') as HTMLInputElement).value).toBe('wrong-password');
  });

  it('keeps the logout confirmation open and visible when logging out fails', async () => {
    const onLogout = vi.fn(async () => {
      throw new ApiError('服务暂时不可用，请稍后重试。', 500, { code: 'internal_error' });
    });
    renderSettings(onLogout);
    openSecuritySection();

    fireEvent.click(screen.getByRole('button', { name: '退出登录' }));
    const confirmation = screen.getByRole('dialog', { name: '退出 wmux？' });
    fireEvent.click(within(confirmation).getByRole('button', { name: '退出登录' }));

    await waitFor(() =>
      expect(within(confirmation).getByRole('alert').textContent).toBe('服务暂时不可用，请稍后重试。'),
    );
    expect(screen.getByRole('dialog', { name: '退出 wmux？' })).toBeTruthy();
    expect(within(confirmation).getByRole('button', { name: '退出登录' }).getAttribute('aria-busy')).toBeNull();
  });
});
