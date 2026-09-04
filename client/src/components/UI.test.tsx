// @vitest-environment jsdom

import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { useState } from 'react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { ActionMenu, Button, ConfirmDialog, Input, Modal } from './UI';

afterEach(cleanup);

describe('Modal', () => {
  it('keeps focus in the form while an inline close callback changes', async () => {
    const user = userEvent.setup();

    function FormDialog() {
      const [value, setValue] = useState('');
      return (
        <Modal open title="编辑" onClose={() => undefined} footer={<Button>保存</Button>}>
          <Input aria-label="名称" value={value} onChange={(event) => setValue(event.target.value)} />
        </Modal>
      );
    }

    render(<FormDialog />);
    const input = screen.getByLabelText('名称') as HTMLInputElement;
    await waitFor(() => expect(document.activeElement).toBe(input));
    await user.type(input, 'abc');
    expect(input.value).toBe('abc');
    expect(document.activeElement).toBe(input);
  });

  it('traps Tab and lets only the top dialog handle Escape', async () => {
    const user = userEvent.setup();
    const closeBottom = vi.fn();
    const closeTop = vi.fn();
    render(
      <>
        <Modal open title="底层" onClose={closeBottom} footer={<Button>底层操作</Button>}>
          <Input aria-label="底层输入" />
        </Modal>
        <Modal open title="顶层" onClose={closeTop} footer={<Button>最后操作</Button>}>
          <Input aria-label="顶层输入" />
        </Modal>
      </>,
    );

    const last = screen.getByRole('button', { name: '最后操作' });
    last.focus();
    await user.tab();
    const topDialog = screen.getByRole('dialog', { name: '顶层' });
    expect(topDialog.contains(document.activeElement)).toBe(true);

    await user.keyboard('{Escape}');
    expect(closeTop).toHaveBeenCalledOnce();
    expect(closeBottom).not.toHaveBeenCalled();
  });
});

describe('ActionMenu and Button', () => {
  function Menu() {
    const [open, setOpen] = useState(false);
    return (
      <ActionMenu open={open} onOpenChange={setOpen} label="更多操作" trigger={<span>…</span>}>
        <button role="menuitem" disabled>
          不可用
        </button>
        <button role="menuitem">重命名</button>
        <button role="menuitem">重启</button>
        <button role="menuitem">删除</button>
      </ActionMenu>
    );
  }

  it('focuses the first enabled item and cycles with menu navigation keys', async () => {
    const user = userEvent.setup();
    render(<Menu />);
    const trigger = screen.getByRole('button', { name: '更多操作' });
    fireEvent.click(trigger);
    expect(trigger.getAttribute('aria-expanded')).toBe('true');
    const menu = screen.getByRole('menu');
    expect(menu.hidden).toBe(false);
    expect(trigger.getAttribute('aria-controls')).toBe(menu.id);

    const rename = screen.getByRole('menuitem', { name: '重命名' });
    const restart = screen.getByRole('menuitem', { name: '重启' });
    const remove = screen.getByRole('menuitem', { name: '删除' });
    await waitFor(() => expect(document.activeElement).toBe(rename));

    await user.keyboard('{ArrowDown}');
    expect(document.activeElement).toBe(restart);
    await user.keyboard('{End}');
    expect(document.activeElement).toBe(remove);
    await user.keyboard('{ArrowDown}');
    expect(document.activeElement).toBe(rename);
    await user.keyboard('{ArrowUp}');
    expect(document.activeElement).toBe(remove);
    await user.keyboard('{Home}');
    expect(document.activeElement).toBe(rename);
  });

  it('closes with Escape or the trigger and restores trigger focus', async () => {
    const user = userEvent.setup();
    render(<Menu />);
    const trigger = screen.getByRole('button', { name: '更多操作' });
    fireEvent.click(trigger);
    const menu = screen.getByRole('menu');
    await waitFor(() => expect(document.activeElement).toBe(screen.getByRole('menuitem', { name: '重命名' })));

    await user.keyboard('{Escape}');
    expect(menu.hidden).toBe(true);
    expect(document.activeElement).toBe(trigger);

    fireEvent.click(trigger);
    await waitFor(() => expect(document.activeElement).toBe(screen.getByRole('menuitem', { name: '重命名' })));
    fireEvent.click(trigger);
    expect(menu.hidden).toBe(true);
    expect(document.activeElement).toBe(trigger);
  });

  it('keeps its label and width-bearing content while busy', () => {
    render(<Button busy>启动会话</Button>);
    const button = screen.getByRole('button', { name: '启动会话' });
    expect(button.textContent).toContain('启动会话');
    expect(button.getAttribute('aria-busy')).toBe('true');
    expect(button.hasAttribute('disabled')).toBe(true);
  });
});

describe('ConfirmDialog', () => {
  it('shows only the caller-provided consequence for a dangerous action', () => {
    render(
      <ConfirmDialog
        open
        title="删除记录？"
        description="将删除这条记录。"
        confirmLabel="删除"
        danger
        onCancel={vi.fn()}
        onConfirm={vi.fn()}
      />,
    );

    expect(screen.getByText('将删除这条记录。')).toBeTruthy();
    expect(screen.queryByText('这个操作无法撤销。')).toBeNull();
    expect(screen.getByRole('button', { name: '删除' }).classList.contains('button--danger')).toBe(true);
  });
});
