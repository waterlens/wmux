// @vitest-environment jsdom

import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { useState } from 'react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { ActionMenu, Button, Input, Modal } from './UI';

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
  it('opens independently of focus and closes with Escape', async () => {
    const user = userEvent.setup();

    function Menu() {
      const [open, setOpen] = useState(false);
      return (
        <ActionMenu open={open} onOpenChange={setOpen} label="更多操作" trigger={<span>…</span>}>
          <button role="menuitem">重命名</button>
        </ActionMenu>
      );
    }

    render(<Menu />);
    const trigger = screen.getByRole('button', { name: '更多操作' });
    fireEvent.click(trigger);
    expect(trigger.getAttribute('aria-expanded')).toBe('true');
    const menu = screen.getByRole('menu');
    expect(menu.hidden).toBe(false);

    await user.keyboard('{Escape}');
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
