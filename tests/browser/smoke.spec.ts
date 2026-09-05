import { createHash } from 'node:crypto';
import { expect, test, type Locator, type Page } from '@playwright/test';
import { desktopViewport, mobileViewport } from '../../playwright.config';

// Only checks that jsdom cannot make: a real xterm against a real PTY/tmux, the
// bracketed-paste and replay-isolation probes, webfont timing, the build
// metadata baked into the binary, and dialogs that collapse under a real layout.
// Plain DOM and copy assertions belong in the vitest suites.

const username = 'browser-smoke';
const password = 'browser-smoke-password';
// Just under the 80-character server-side name limit, to exercise long names in
// sidebar buttons and dialog titles.
const longName = `wmux-${'x'.repeat(69)}`;

const consoleErrors: string[] = [];

test.beforeEach(({ page }) => {
  consoleErrors.length = 0;
  page.on('console', (message) => {
    if (message.type() === 'error') consoleErrors.push(`console: ${message.text()}`);
  });
  page.on('pageerror', (error) => consoleErrors.push(`page: ${error.message}`));
});

test.afterEach(() => {
  expect(consoleErrors).toEqual([]);
});

async function signIn(page: Page) {
  await page.goto('/');
  await page.getByLabel('用户名').fill(username);
  await page.getByPlaceholder('输入密码', { exact: true }).fill(password);
  const setup = page.getByRole('button', { name: '完成设置' });
  if (await setup.count()) {
    await page.getByPlaceholder('再次输入密码', { exact: true }).fill(password);
    await setup.click();
  } else {
    await page.getByRole('button', { name: '进入工作台' }).click();
  }
  await expect(page.getByRole('heading', { name: '终端会话' })).toBeVisible();
}

/** Creates a session and resolves once its terminal has finished replaying. */
async function startSession(page: Page, name: string, persistence: 'tmux' | 'none' = 'tmux') {
  await page
    .getByRole('button', { name: /新建会话/ })
    .first()
    .click();
  const dialog = page.getByRole('dialog', { name: '新建会话' });
  await dialog.getByLabel('会话名称').fill(name);
  await dialog.getByLabel('持久化方式').selectOption(persistence);
  await dialog.getByRole('button', { name: '启动会话' }).click();
  await expect(page.locator('.terminal-view.is-active[data-replay-complete="true"]')).toBeVisible();
  return page.locator('.terminal-view.is-active .xterm-helper-textarea');
}

async function terminateSession(page: Page, name: string) {
  await page.getByRole('button', { name: `结束会话 ${name}` }).click();
  const confirm = page.getByRole('dialog', { name: new RegExp(name) });
  await confirm.getByRole('button', { name: '结束会话' }).click();
  await expect(confirm).toBeHidden();
}

/** Runs a probe without letting the shell or tmux rewrite its source. */
function pythonCommand(source: string) {
  const encoded = Buffer.from(source).toString('base64');
  return `python3 -c "import base64;exec(base64.b64decode('${encoded}'))"`;
}

/**
 * Layout-collapse check only: no pixel or token values, because those live in
 * `client/src/styles.css` and are covered by the component tests.
 */
async function expectDialogUsable(dialog: Locator, { bodyMustFit = false } = {}) {
  await expect(dialog).toBeVisible();
  const problems = await dialog.evaluate((panel, mustFit) => {
    const found: string[] = [];
    const rect = panel.getBoundingClientRect();
    if (
      rect.left < -1 ||
      rect.top < -1 ||
      rect.right > globalThis.innerWidth + 1 ||
      rect.bottom > globalThis.innerHeight + 1 ||
      panel.scrollWidth - panel.clientWidth > 1
    ) {
      found.push('panel overflows the viewport');
    }
    const close = panel.querySelector('[data-modal-close]');
    const closeRect = close?.getBoundingClientRect();
    if (!close || !closeRect) {
      found.push('panel has no close button');
    } else {
      const hit = globalThis.document.elementFromPoint(
        closeRect.x + closeRect.width / 2,
        closeRect.y + closeRect.height / 2,
      );
      if (!close.contains(hit)) found.push('close button is clipped or obstructed');
      const title = panel.querySelector('.modal__header h2');
      if (
        title &&
        (title.scrollWidth - title.clientWidth > 1 || title.getBoundingClientRect().right > closeRect.left + 1)
      )
        found.push('title overflows or crowds the close button');
    }
    const body = panel.querySelector('.modal__body');
    if (mustFit && (!body || body.scrollHeight > body.clientHeight + 1)) found.push('form requires internal scrolling');
    return found;
  }, bodyMustFit);
  expect(problems).toEqual([]);
}

test('the built binary serves its injected version and commit', async ({ request }) => {
  const response = await request.get('/api/status');
  expect(response.ok()).toBeTruthy();
  const status = (await response.json()) as { version?: string; commit?: string };
  expect(status.commit).toBeTruthy();
  // `scripts/build-server.sh` must have replaced the source-level fallback.
  expect(status.version).toBeTruthy();
  expect(status.version).not.toBe('dev');
});

test('xterm opens after its webfonts and round-trips through a real tmux PTY', async ({ page }) => {
  const fontReleases: number[] = [];
  await page.route('**/*.woff2', async (route) => {
    const pathname = decodeURIComponent(new URL(route.request().url()).pathname).toLowerCase();
    if (!pathname.includes('jetbrains-mono') && !pathname.includes('symbolsnerdfontmono')) {
      await route.continue();
      return;
    }
    const response = await route.fetch();
    await new Promise((wait) => setTimeout(wait, 350));
    fontReleases.push(Date.now());
    await route.fulfill({ response });
  });

  await signIn(page);
  // Armed before any terminal exists, so it times the first xterm.open().
  const openedAt = page.evaluate(
    () =>
      new Promise<number>((resolve) => {
        const observer = new globalThis.MutationObserver(() => {
          if (globalThis.document.querySelector('.terminal-view.is-active .xterm')) {
            observer.disconnect();
            resolve(Date.now());
          }
        });
        observer.observe(globalThis.document.documentElement, { childList: true, subtree: true });
      }),
  );

  const input = await startSession(page, '浏览器验收');
  expect(fontReleases.length).toBeGreaterThan(0);
  expect(await openedAt).toBeGreaterThanOrEqual(Math.max(...fontReleases));
  await expect
    .poll(() =>
      page.evaluate(() => ({
        normal: globalThis.document.fonts.check('14px "JetBrains Mono Variable"', 'W'),
        italic: globalThis.document.fonts.check('italic 14px "JetBrains Mono Variable"', 'W'),
        nerd: globalThis.document.fonts.check('14px "Symbols Nerd Font Mono"', '\ue0b0'),
      })),
    )
    .toEqual({ normal: true, italic: true, nerd: true });

  const marker = 'WMUX_BROWSER_OUTPUT_OK';
  await input.focus();
  await page.keyboard.type(pythonCommand(`print('${marker}')`), { delay: 1 });
  await page.keyboard.press('Enter');
  await expect(page.locator('.terminal-view.is-active .xterm-rows > div').filter({ hasText: marker })).toBeVisible();
  await expect
    .poll(() =>
      page.evaluate(async () => {
        const sessions = (await (await fetch('/api/sessions')).json()) as { backend: string }[];
        return sessions.map((session) => session.backend);
      }),
    )
    .toEqual(['tmux']);

  await page.setViewportSize(mobileViewport);
  await expect
    .poll(() => page.evaluate(() => globalThis.document.documentElement.scrollWidth - globalThis.innerWidth))
    .toBeLessThanOrEqual(1);
  const mobileTerminal = await page.locator('.terminal-view.is-active').boundingBox();
  expect(mobileTerminal?.width).toBeGreaterThan(300);
  expect(mobileTerminal?.height).toBeGreaterThan(500);

  await page.setViewportSize(desktopViewport);
  await terminateSession(page, '浏览器验收');
});

test('a bracketed paste larger than 128 KiB reaches the PTY intact', async ({ page }) => {
  const pasteText = 'wmux-汉🙂\n'.repeat(12_000);
  const pasteBody = Buffer.from(pasteText.replace(/\r?\n/g, '\r'));
  expect(pasteBody.length).toBeGreaterThan(128 * 1024);
  const pasteHash = createHash('sha256').update(pasteBody).digest('hex');

  await signIn(page);
  const input = await startSession(page, '大块粘贴');
  // The toolbar paste button only exists in the phone layout.
  await page.setViewportSize(mobileViewport);
  await input.focus();
  await page.keyboard.insertText(
    pythonCommand(String.raw`
import hashlib, os, select, termios, time, tty
old = termios.tcgetattr(0)
try:
    tty.setraw(0)
    os.write(1, b'\x1b[?2004h\r\nPASTE_ARMED\r\n')
    expected_body = ${pasteBody.length}
    expected_total = expected_body + 12
    data = bytearray()
    deadline = time.monotonic() + 15
    while len(data) < expected_total:
        timeout = deadline - time.monotonic()
        if timeout <= 0 or not select.select([0], [], [], timeout)[0]:
            break
        chunk = os.read(0, min(65536, expected_total - len(data)))
        if not chunk:
            break
        data.extend(chunk)
    packet = bytes(data)
    framed = packet.startswith(b'\x1b[200~') and packet.endswith(b'\x1b[201~')
    body = packet[6:-6] if framed else b''
    valid = framed and len(body) == expected_body and hashlib.sha256(body).hexdigest() == '${pasteHash}' and b'\n' not in body
    os.write(1, b'\x1b[?2004l\r\n' + (b'PASTE_OK' if valid else ('PASTE_BAD:' + str(len(packet)) + ':' + hashlib.sha256(body).hexdigest()).encode()) + b'\r\n')
finally:
    termios.tcsetattr(0, termios.TCSADRAIN, old)
`),
  );
  await page.keyboard.press('Enter');
  const rows = page.locator('.terminal-view.is-active .xterm-rows');
  await expect(rows.filter({ hasText: 'PASTE_ARMED' })).toBeVisible();

  await page.evaluate((text) => globalThis.navigator.clipboard.writeText(text), pasteText);
  await page.getByRole('button', { name: '粘贴', exact: true }).click();
  await expect(rows.filter({ hasText: /PASTE_(?:OK|BAD)/ })).toContainText('PASTE_OK', { timeout: 30_000 });

  await terminateSession(page, '大块粘贴');
});

test('replayed history never answers terminal queries back into the PTY', async ({ page }) => {
  await signIn(page);
  const input = await startSession(page, '重放隔离');
  await input.focus();
  await page.keyboard.insertText(
    pythonCommand(String.raw`
import os, select, termios, tty
old = termios.tcgetattr(0)
try:
    tty.setraw(0)
    os.write(1, b'\x1b[5n')
    if not select.select([0], [], [], 5)[0]:
        os.write(1, b'\r\nREPLAY_SETUP_FAILED\r\n')
    else:
        first = os.read(0, 32)
        os.write(1, b'\r\nREPLAY_ARMED:' + first.hex().encode() + b'\r\n')
        if select.select([0], [], [], 15)[0]:
            leaked = os.read(0, 32)
            os.write(1, b'\r\nREPLAY_LEAK:' + leaked.hex().encode() + b'\r\n')
        else:
            os.write(1, b'\r\nREPLAY_SAFE\r\n')
finally:
    termios.tcsetattr(0, termios.TCSADRAIN, old)
`),
  );
  await page.keyboard.press('Enter');
  const rows = page.locator('.terminal-view.is-active .xterm-rows');
  // The live terminal must answer CSI 5n, otherwise the probe proves nothing.
  await expect(rows.filter({ hasText: /REPLAY_(?:ARMED|SETUP_FAILED)/ })).toContainText('REPLAY_ARMED');

  await page.reload({ waitUntil: 'domcontentloaded' });
  await expect(page.locator('.terminal-view.is-active[data-replay-complete="true"]')).toBeVisible({ timeout: 30_000 });
  await expect(rows.filter({ hasText: /REPLAY_(?:SAFE|LEAK)/ })).toContainText('REPLAY_SAFE', { timeout: 30_000 });

  await terminateSession(page, '重放隔离');
});

test('dialogs stay usable on desktop and phone viewports', async ({ page }) => {
  await signIn(page);

  await page.getByRole('button', { name: /管理 SSH 主机/ }).click();
  await page.getByRole('button', { name: '添加主机', exact: true }).click();
  const hostEditor = page.getByRole('dialog', { name: '添加 SSH 主机' });
  await expectDialogUsable(hostEditor, { bodyMustFit: true });
  await page.setViewportSize(mobileViewport);
  await expectDialogUsable(hostEditor);
  await hostEditor.getByRole('button', { name: '关闭' }).click();
  await page.setViewportSize(desktopViewport);

  // A long session name must not push the title over the close button or grow
  // the confirmation past the narrowest supported viewport.
  await startSession(page, longName, 'none');
  await page.getByRole('button', { name: `结束会话 ${longName}` }).click();
  const confirm = page.getByRole('dialog', { name: new RegExp(longName) });
  await expectDialogUsable(confirm);
  await page.setViewportSize({ width: 320, height: 568 });
  await expectDialogUsable(confirm);
  await confirm.getByRole('button', { name: '结束会话' }).click();
  await expect(confirm).toBeHidden();
});

test('a fixed column count keeps its width on the phone by shrinking the font', async ({ page }) => {
  await page.addInitScript(() => {
    globalThis.localStorage.setItem(
      'wmux.terminalPreferences',
      JSON.stringify({
        fontFamily: 'jetbrains-mono',
        fontSize: 14,
        columns: 80,
        cursorStyle: 'block',
        cursorBlink: true,
        scrollback: 10_000,
        theme: 'light',
      }),
    );
  });
  await signIn(page);
  const input = await startSession(page, '固定宽度');
  const rows = page.locator('.terminal-view.is-active .xterm-rows');
  const canvas = page.locator('.terminal-view.is-active .terminal-canvas');
  const geometry = () =>
    canvas.evaluate((mount) => {
      const screen = mount.querySelector<HTMLElement>('.xterm-screen');
      return {
        mount: mount.clientWidth,
        screen: screen?.offsetWidth ?? 0,
        margin: Number.parseFloat(screen?.style.marginLeft || '0'),
        overflow: mount.scrollWidth - mount.clientWidth,
      };
    });

  await input.focus();
  await page.keyboard.type(pythonCommand(`import os; print('COLS=%d' % os.get_terminal_size().columns)`), {
    delay: 1,
  });
  await page.keyboard.press('Enter');
  await expect(rows.filter({ hasText: 'COLS=80' })).toBeVisible();
  // On a wide desktop the 80 columns keep the preferred font and sit centred in the pane.
  const desktop = await geometry();
  expect(desktop.screen).toBeLessThan(desktop.mount - 100);
  expect(desktop.margin).toBeGreaterThan(20);

  await page.setViewportSize(mobileViewport);
  await expect.poll(async () => (await geometry()).overflow).toBeLessThanOrEqual(1);
  await expect.poll(async () => (await geometry()).screen).toBeGreaterThan(0);
  const phone = await geometry();
  expect(phone.screen).toBeLessThanOrEqual(phone.mount);
  await input.focus();
  await page.keyboard.type(pythonCommand(`import os; print('PHONE_COLS=%d' % os.get_terminal_size().columns)`), {
    delay: 1,
  });
  await page.keyboard.press('Enter');
  await expect(rows.filter({ hasText: 'PHONE_COLS=80' })).toBeVisible();

  await page.setViewportSize(desktopViewport);
  await terminateSession(page, '固定宽度');
});
