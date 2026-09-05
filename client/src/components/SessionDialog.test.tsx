// @vitest-environment jsdom

import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { SessionDialog } from './SessionDialog';

afterEach(cleanup);

describe('SessionDialog copy', () => {
  it('exposes and updates the selected run location', () => {
    render(<SessionDialog open hosts={[]} sessions={[]} onClose={vi.fn()} onCreated={vi.fn()} />);

    const group = screen.getByRole('group', { name: '运行位置' });
    const local = screen.getByRole('button', { name: /本机/ });
    const ssh = screen.getByRole('button', { name: /SSH 主机/ });
    expect(group.contains(local)).toBe(true);
    expect(group.contains(ssh)).toBe(true);
    expect(local.getAttribute('aria-pressed')).toBe('true');
    expect(ssh.getAttribute('aria-pressed')).toBe('false');

    fireEvent.click(ssh);
    expect(local.getAttribute('aria-pressed')).toBe('false');
    expect(ssh.getAttribute('aria-pressed')).toBe('true');
  });

  it('keeps the default form quiet and only explains non-persistent mode', () => {
    render(<SessionDialog open hosts={[]} sessions={[]} onClose={vi.fn()} onCreated={vi.fn()} />);

    expect(screen.queryByText('进程将在浏览器关闭后继续运行。')).toBeNull();
    expect(screen.queryByText(/断线后会自动重连/)).toBeNull();
    expect(screen.queryByText('不持久化会话会在服务连接终止时结束。')).toBeNull();
    expect(screen.getByText('自动选择可用后端')).toBeTruthy();

    fireEvent.change(screen.getByRole('combobox'), { target: { value: 'none' } });
    const note = screen.getByText('不持久化会话会在服务连接终止时结束。');
    expect(note.classList.contains('persistence-note')).toBe(true);
    expect(document.querySelector('.info-callout')).toBeNull();
    expect(screen.queryByText('自动选择可用后端')).toBeNull();

    fireEvent.change(screen.getByRole('combobox'), { target: { value: 'tmux' } });
    expect(screen.queryByText('自动选择可用后端')).toBeNull();
    expect(screen.queryByText('不持久化会话会在服务连接终止时结束。')).toBeNull();

    fireEvent.change(screen.getByRole('combobox'), { target: { value: 'auto' } });
    expect(screen.queryByText('不持久化会话会在服务连接终止时结束。')).toBeNull();
    expect(screen.getByText('自动选择可用后端')).toBeTruthy();
  });
});
