// @vitest-environment jsdom

import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { useState } from 'react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { ActionMenu, Button, ConfirmDialog, Input, Modal } from './UI';

afterEach(cleanup);

describe('Modal', () => {
  it('omits the body and footer regions until content is provided', () => {
    const { rerender } = render(<Modal open title="确认" onClose={vi.fn()} />);
    const dialog = screen.getByRole('dialog', { name: '确认' });
    expect(dialog.querySelector('.modal__body')).toBeNull();
    expect(dialog.querySelector('.modal__footer')).toBeNull();

    rerender(
      <Modal open title="确认" onClose={vi.fn()} footer={<Button>完成</Button>}>
        <p>请核对指纹。</p>
      </Modal>,
    );
    expect(dialog.querySelector('.modal__body')?.textContent).toBe('请核对指纹。');
    expect(screen.getByRole('button', { name: '完成' })).toBeTruthy();
  });

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

  it('hides the lower modal from interaction and restores it after a nested modal closes', () => {
    function NestedDialogs() {
      const [confirmOpen, setConfirmOpen] = useState(false);
      return (
        <>
          <Modal open title="设置" onClose={vi.fn()}>
            <Button onClick={() => setConfirmOpen(true)}>退出登录</Button>
          </Modal>
          <ConfirmDialog
            open={confirmOpen}
            title="退出？"
            description="将断开当前浏览器。"
            onCancel={() => setConfirmOpen(false)}
            onConfirm={vi.fn()}
          />
        </>
      );
    }

    render(<NestedDialogs />);
    const settings = screen.getByRole('dialog', { name: '设置' });
    const lowerLayer = settings.parentElement!;
    const trigger = screen.getByRole('button', { name: '退出登录' });
    trigger.focus();
    fireEvent.click(trigger);

    expect(lowerLayer.inert).toBe(true);
    expect(lowerLayer.getAttribute('aria-hidden')).toBe('true');
    expect(screen.queryByRole('dialog', { name: '设置' })).toBeNull();
    expect(screen.getAllByRole('dialog')).toHaveLength(1);

    fireEvent.click(screen.getByRole('button', { name: '取消' }));
    expect(lowerLayer.inert).toBe(false);
    expect(lowerLayer.hasAttribute('aria-hidden')).toBe(false);
    expect(screen.getByRole('dialog', { name: '设置' })).toBe(settings);
    expect(document.activeElement).toBe(trigger);
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
  it('blocks every dismissal path while busy and restores dismissal afterward', () => {
    const onCancel = vi.fn();
    const props = {
      open: true,
      title: '结束会话？',
      description: '将结束进程。',
      onCancel,
      onConfirm: vi.fn(),
    };
    const { rerender } = render(<ConfirmDialog {...props} busy />);
    const dialog = screen.getByRole('dialog', { name: '结束会话？' });
    const close = screen.getByRole('button', { name: '关闭' });
    expect(close.hasAttribute('disabled')).toBe(true);
    fireEvent.click(close);
    fireEvent.click(screen.getByRole('button', { name: '取消' }));
    fireEvent.mouseDown(dialog.parentElement!);
    fireEvent.keyDown(document, { key: 'Escape' });
    expect(onCancel).not.toHaveBeenCalled();

    rerender(<ConfirmDialog {...props} busy={false} />);
    expect(close.hasAttribute('disabled')).toBe(false);
    fireEvent.keyDown(document, { key: 'Escape' });
    expect(onCancel).toHaveBeenCalledOnce();
  });

  it('renders the caller-provided consequence and wires both actions', () => {
    const cancel = vi.fn();
    const confirm = vi.fn();
    render(
      <ConfirmDialog
        open
        title="删除记录？"
        description="将删除这条记录。"
        confirmLabel="删除"
        danger
        onCancel={cancel}
        onConfirm={confirm}
      />,
    );

    expect(screen.getByText('将删除这条记录。')).toBeTruthy();
    const dialog = screen.getByRole('dialog', { name: '删除记录？' });
    expect(dialog.querySelector('.modal__body')).toBeNull();
    expect(document.getElementById(dialog.getAttribute('aria-describedby') ?? '')?.textContent).toBe(
      '将删除这条记录。',
    );
    expect(screen.getByRole('button', { name: '删除' }).classList.contains('button--danger')).toBe(true);
    fireEvent.click(screen.getByRole('button', { name: '取消' }));
    expect(cancel).toHaveBeenCalledOnce();
    expect(confirm).not.toHaveBeenCalled();
    fireEvent.click(screen.getByRole('button', { name: '删除' }));
    expect(confirm).toHaveBeenCalledOnce();
  });
});
