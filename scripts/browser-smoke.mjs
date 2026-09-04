import { spawn } from 'node:child_process';
import { createHash } from 'node:crypto';
import { access, mkdir, mkdtemp, rm, writeFile } from 'node:fs/promises';
import { createServer } from 'node:net';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';
import { chromium } from 'playwright-core';

const projectDir = resolve(import.meta.dirname, '..');
const username = process.env.WMUX_BROWSER_USERNAME ?? 'browser-smoke';
const password = process.env.WMUX_BROWSER_PASSWORD ?? 'browser-smoke-password';
const outputDir = resolve(
  process.env.WMUX_BROWSER_OUTPUT_DIR ?? (await mkdtemp(join(tmpdir(), 'wmux-browser-output-'))),
);
await mkdir(outputDir, { recursive: true });

const browserErrors = [];
const delayedTerminalFonts = [];
let ownedServer;
let ownedDataDir;
let baseURL = process.env.WMUX_BROWSER_URL;
let buildStatus;
const executablePath = await findChrome();
let browser;
let pasteBytes = 0;
let replayIsolation = 'not-run';
let sshConfigImport = 'not-run';

try {
  if (!baseURL) {
    const binary = resolve(process.env.WMUX_BROWSER_BINARY ?? join(projectDir, 'dist', 'wmux'));
    await access(binary).catch(() => {
      throw new Error(`wmux binary not found at ${binary}; run pnpm build first`);
    });
    const port = await availablePort();
    baseURL = `http://127.0.0.1:${port}`;
    ownedDataDir = await mkdtemp(join(tmpdir(), 'wmux-browser-data-'));
    const sshConfigDir = join(ownedDataDir, 'ssh-config');
    const sshConfigIncludes = join(sshConfigDir, 'config.d');
    const sshConfigPath = join(sshConfigDir, 'config');
    await mkdir(sshConfigIncludes, { recursive: true });
    await writeFile(
      sshConfigPath,
      `Include "${join(sshConfigIncludes, '*.conf')}"
Host review-*
  User inherited-user
  Port 2200
Host *
  User fallback-user
  Port 22
`,
      { mode: 0o600 },
    );
    await writeFile(
      join(sshConfigIncludes, 'hosts.conf'),
      `Host review-box
  HostName 192.0.2.44
  IdentityFile /private/wmux-browser-must-not-read
Host proxy-box
  HostName 192.0.2.45
  ProxyJump bastion
`,
      { mode: 0o600 },
    );
    ownedServer = spawn(binary, [], {
      cwd: projectDir,
      env: {
        ...process.env,
        WMUX_HOST: '127.0.0.1',
        WMUX_PORT: String(port),
        WMUX_DATA_DIR: ownedDataDir,
        WMUX_SSH_CONFIG: sshConfigPath,
        WMUX_PUBLIC_URL: baseURL,
        WMUX_LOG_LEVEL: 'warn',
      },
      stdio: ['ignore', 'pipe', 'pipe'],
    });
    ownedServer.stdout.on('data', (chunk) => browserErrors.push(`server stdout: ${chunk}`));
    ownedServer.stderr.on('data', (chunk) => {
      const message = String(chunk);
      if (/level=(ERROR|WARN)/.test(message)) browserErrors.push(`server: ${message.trim()}`);
    });
    await waitForHealth(`${baseURL}/api/health`, ownedServer);
  }

  const statusResponse = await fetch(`${baseURL}/api/status`);
  if (!statusResponse.ok) throw new Error(`status endpoint returned ${statusResponse.status}`);
  buildStatus = await statusResponse.json();
  if (typeof buildStatus.version !== 'string' || !buildStatus.version) {
    throw new Error(`status endpoint has no build version: ${JSON.stringify(buildStatus)}`);
  }
  if (ownedServer && buildStatus.version === 'dev') {
    throw new Error('the locally built binary still reports the development fallback version');
  }
  if (typeof buildStatus.commit !== 'string' || !buildStatus.commit) {
    throw new Error(`status endpoint has no build commit: ${JSON.stringify(buildStatus)}`);
  }

  browser = await chromium.launch({ executablePath, headless: true });
  const context = await browser.newContext({ viewport: { width: 1440, height: 960 } });
  if (ownedServer) {
    await context.grantPermissions(['clipboard-read', 'clipboard-write'], { origin: new URL(baseURL).origin });
  }
  const page = await context.newPage();

  if (ownedServer) {
    await page.route('**/*.woff2', async (route) => {
      const pathname = decodeURIComponent(new URL(route.request().url()).pathname).toLowerCase();
      if (!pathname.includes('jetbrains-mono') && !pathname.includes('symbolsnerdfontmono')) {
        await route.continue();
        return;
      }
      const timing = { pathname, requestedAt: Date.now(), releasedAt: 0 };
      delayedTerminalFonts.push(timing);
      const response = await route.fetch();
      await new Promise((resolveWait) => setTimeout(resolveWait, 350));
      timing.releasedAt = Date.now();
      await route.fulfill({ response });
    });
  }

  page.on('console', (message) => {
    if (message.type() === 'error') browserErrors.push(`console: ${message.text()}`);
  });
  page.on('pageerror', (error) => browserErrors.push(`page: ${error.message}`));

  await page.goto(baseURL, { waitUntil: 'networkidle' });
  await page.screenshot({ path: join(outputDir, 'auth.png'), fullPage: true });
  await page.getByLabel('用户名').fill(username);
  await page.getByPlaceholder('输入密码', { exact: true }).fill(password);
  if (await page.getByRole('button', { name: '完成设置' }).count()) {
    await page.getByPlaceholder('再次输入密码', { exact: true }).fill(password);
    await page.getByRole('button', { name: '完成设置' }).click();
  } else {
    await page.getByRole('button', { name: '进入工作台' }).click();
  }
  await page.getByRole('heading', { name: '终端会话' }).waitFor();
  await page.screenshot({ path: join(outputDir, 'dashboard.png'), fullPage: true });

  const desktopSidebarActions = page.locator('.sidebar__header-actions button:visible');
  if ((await desktopSidebarActions.count()) !== 1) {
    throw new Error(`desktop sidebar exposed ${await desktopSidebarActions.count()} header actions`);
  }
  if ((await desktopSidebarActions.first().getAttribute('aria-label')) !== '收起侧栏') {
    throw new Error('desktop sidebar did not expose only the collapse action');
  }
  const sidebarCounterCount = await page
    .locator('.sidebar-section-title > span:not(:first-child), .session-group__header em, .sidebar-nav > em')
    .count();
  if (sidebarCounterCount !== 0) throw new Error(`sidebar still exposes ${sidebarCounterCount} numeric counters`);
  const accountDivider = await page.locator('.sidebar-account').evaluate((element) => {
    const style = globalThis.getComputedStyle(element);
    return { width: style.borderTopWidth, style: style.borderTopStyle };
  });
  if (accountDivider.width === '0px' || accountDivider.style === 'none') {
    throw new Error(`sidebar account area has no divider: ${JSON.stringify(accountDivider)}`);
  }

  await page.getByRole('button', { name: /管理 SSH 主机/ }).click();
  await page.getByRole('heading', { name: 'SSH 主机' }).waitFor();
  await page.screenshot({ path: join(outputDir, 'hosts.png'), fullPage: true });

  for (const redundantText of [
    '主机密钥校验始终开启',
    'wmux 不会静默接受新指纹。主机重装或密钥变化后，需要你重新确认。',
  ]) {
    if (await page.getByText(redundantText, { exact: true }).count()) {
      throw new Error(`host manager still exposes redundant copy: ${redundantText}`);
    }
  }

  await page.getByRole('button', { name: '添加主机', exact: true }).click();
  const hostEditor = page.getByRole('dialog', { name: '添加 SSH 主机' });
  await hostEditor.waitFor();
  for (const redundantText of ['凭据由 wmux 服务加密保管。', '支持 OpenSSH PEM 格式']) {
    if (await hostEditor.getByText(redundantText, { exact: true }).count()) {
      throw new Error(`host editor still exposes redundant copy: ${redundantText}`);
    }
  }
  if (await hostEditor.getByText(/保存后还需要探测并确认主机指纹/).count()) {
    throw new Error('host editor still duplicates the post-save fingerprint flow');
  }
  await hostEditor.getByRole('button', { name: '关闭' }).click();

  if (ownedServer) {
    const discovery = await page.evaluate(async () => {
      const response = await fetch('/api/hosts/ssh-config', { credentials: 'same-origin' });
      return { status: response.status, body: await response.json() };
    });
    const reviewHost = discovery.body.candidates?.find((candidate) => candidate.alias === 'review-box');
    const proxyHost = discovery.body.candidates?.find((candidate) => candidate.alias === 'proxy-box');
    if (
      discovery.status !== 200 ||
      !discovery.body.available ||
      reviewHost?.address !== '192.0.2.44' ||
      reviewHost?.username !== 'inherited-user' ||
      reviewHost?.port !== 2200 ||
      reviewHost?.hasIdentityFile !== true ||
      reviewHost?.unsupported?.length !== 0
    ) {
      throw new Error(`SSH config Include/inheritance discovery failed: ${JSON.stringify(discovery)}`);
    }
    if (!proxyHost?.unsupported?.includes('ProxyJump')) {
      throw new Error(`ProxyJump was not marked unsupported: ${JSON.stringify(proxyHost)}`);
    }
    if (JSON.stringify(discovery).includes('wmux-browser-must-not-read')) {
      throw new Error('SSH config discovery leaked an IdentityFile path');
    }

    await page.getByRole('button', { name: '从 SSH config 导入主机' }).click();
    const importDialog = page.getByRole('dialog', { name: '从 SSH config 导入' });
    await importDialog.waitFor();
    await importDialog.getByText('inherited-user@192.0.2.44:2200', { exact: true }).waitFor();
    await page.screenshot({ path: join(outputDir, 'ssh-config-import.png'), fullPage: true });
    const proxyImport = importDialog.getByRole('button', { name: '导入 proxy-box' });
    if (!(await proxyImport.isDisabled()) || !(await importDialog.getByText('暂不支持 ProxyJump').count())) {
      throw new Error('unsupported ProxyJump candidate remained importable or unlabeled');
    }

    const probeRequests = [];
    const recordProbe = (request) => {
      if (/\/api\/hosts\/[^/]+\/probe$/.test(new URL(request.url()).pathname)) probeRequests.push(request.url());
    };
    page.on('request', recordProbe);
    const importedResponse = page.waitForResponse(
      (response) =>
        response.request().method() === 'POST' && new URL(response.url()).pathname === '/api/hosts/import-ssh-config',
    );
    await importDialog.getByRole('button', { name: '导入 review-box' }).click();
    const imported = await importedResponse;
    if (imported.status() !== 201) throw new Error(`SSH config import returned ${imported.status()}`);
    await importDialog.getByRole('button', { name: 'review-box 已导入' }).waitFor();
    sshConfigImport = 'review-box';
    await importDialog.getByRole('button', { name: '完成' }).click();
    const importedCard = page.locator('.host-card').filter({ has: page.getByRole('heading', { name: 'review-box' }) });
    await importedCard.getByText('待验证指纹', { exact: true }).waitFor();
    if (!(await importedCard.getByRole('button', { name: '新建会话' }).isDisabled())) {
      throw new Error('imported SSH host was trusted without fingerprint verification');
    }
    page.off('request', recordProbe);
    if (probeRequests.length)
      throw new Error(`SSH config import unexpectedly probed a host: ${probeRequests.join(', ')}`);
  }

  await page
    .locator('.sidebar__footer')
    .getByRole('button', { name: new RegExp(username) })
    .click();
  const settingsDialog = page.getByRole('dialog', { name: '设置' });
  await settingsDialog.waitFor();
  await page.screenshot({ path: join(outputDir, 'settings.png'), fullPage: true });
  await settingsDialog.getByRole('button', { name: '关闭' }).click();

  await page
    .getByRole('button', { name: /新建会话/ })
    .first()
    .click();
  const dialog = page.getByRole('dialog', { name: '新建会话' });
  if (await dialog.getByText('进程将在浏览器关闭后继续运行。', { exact: true }).count()) {
    throw new Error('new-session dialog still repeats browser-close persistence copy');
  }
  if (await dialog.getByText(/断线后会自动重连/).count()) {
    throw new Error('new-session dialog still shows the default reconnect callout');
  }
  await dialog.getByLabel('持久化方式').selectOption('none');
  await dialog.getByText('不持久化会话会在服务连接终止时结束。', { exact: true }).waitFor();
  await dialog.getByLabel('会话名称').fill('浏览器验收');
  await dialog.getByLabel('持久化方式').selectOption(ownedServer ? 'tmux' : 'none');
  await page.evaluate(() => {
    globalThis.__wmuxTerminalOpenedAt = 0;
    const observer = new globalThis.MutationObserver(() => {
      if (!globalThis.__wmuxTerminalOpenedAt && globalThis.document.querySelector('.terminal-view.is-active .xterm')) {
        globalThis.__wmuxTerminalOpenedAt = Date.now();
        observer.disconnect();
      }
    });
    observer.observe(globalThis.document.documentElement, { childList: true, subtree: true });
  });
  await dialog.getByRole('button', { name: '启动会话' }).click();

  await page.locator('.terminal-view.is-active .xterm-helper-textarea').waitFor();
  await page.getByText('已连接', { exact: true }).waitFor();
  await page.locator('.terminal-view.is-active[data-replay-complete="true"]').waitFor();

  const stableStatusIcons = await page.locator('.terminal-view.is-active .live-status svg').count();
  if (stableStatusIcons !== 0)
    throw new Error(`stable terminal status still has ${stableStatusIcons} duplicate icon(s)`);
  if ((await page.locator('.terminal-view.is-active .connection-dot').count()) !== 1) {
    throw new Error('terminal toolbar does not expose exactly one connection status dot');
  }
  const terminateButton = page.getByRole('button', { name: '结束会话 浏览器验收' });
  if ((await terminateButton.getAttribute('title')) !== '结束会话') {
    throw new Error('icon-only terminate action has no tooltip');
  }
  const toolButtonStyles = await page.evaluate(() => {
    const root = globalThis.document.querySelector('.terminal-view.is-active');
    const copy = root?.querySelector('.tool-button[title="复制选中内容"]');
    const terminate = root?.querySelector('.terminate-session-button');
    if (!(copy instanceof globalThis.HTMLElement) || !(terminate instanceof globalThis.HTMLElement)) return null;
    const copyStyle = globalThis.getComputedStyle(copy);
    const terminateStyle = globalThis.getComputedStyle(terminate);
    return {
      copy: { width: copyStyle.width, height: copyStyle.height },
      terminate: {
        width: terminateStyle.width,
        height: terminateStyle.height,
        background: terminateStyle.backgroundColor,
        color: terminateStyle.color,
      },
    };
  });
  if (
    !toolButtonStyles ||
    toolButtonStyles.copy.width !== toolButtonStyles.terminate.width ||
    toolButtonStyles.copy.height !== toolButtonStyles.terminate.height ||
    !['rgba(0, 0, 0, 0)', 'transparent'].includes(toolButtonStyles.terminate.background)
  ) {
    throw new Error(`terminate action is not a same-size transparent tool button: ${JSON.stringify(toolButtonStyles)}`);
  }

  if (ownedServer) {
    if (!delayedTerminalFonts.length) throw new Error('terminal font requests were not observed');
    const openedAt = await page.evaluate(() => globalThis.__wmuxTerminalOpenedAt);
    const lastFontRelease = Math.max(...delayedTerminalFonts.map(({ releasedAt }) => releasedAt));
    if (!openedAt || openedAt < lastFontRelease) {
      throw new Error(
        `xterm opened before terminal fonts were released: open=${openedAt}, fonts=${JSON.stringify(delayedTerminalFonts)}`,
      );
    }
    const loadedFonts = await page.evaluate(() => ({
      normal: globalThis.document.fonts.check('14px "JetBrains Mono Variable"', 'W'),
      italic: globalThis.document.fonts.check('italic 14px "JetBrains Mono Variable"', 'W'),
      nerd: globalThis.document.fonts.check('14px "Symbols Nerd Font Mono"', '\ue0b0'),
    }));
    if (!loadedFonts.normal || !loadedFonts.italic || !loadedFonts.nerd) {
      throw new Error(`terminal fonts are not ready after xterm.open: ${JSON.stringify(loadedFonts)}`);
    }
    await page.waitForFunction(async () => {
      const response = await fetch('/api/sessions', { credentials: 'same-origin' });
      const sessions = await response.json();
      return (
        Array.isArray(sessions) &&
        sessions.some((session) => session.name === '浏览器验收' && session.backend === 'tmux')
      );
    });
  }

  const terminalInput = page.locator('.terminal-view.is-active .xterm-helper-textarea');
  await terminalInput.focus();
  const commandMarker = 'WMUX_BROWSER_OUTPUT_OK';
  await page.keyboard.type(encodedPythonCommand(`print('${commandMarker}')`), { delay: 1 });
  await page.keyboard.press('Enter');
  await page.locator('.terminal-view.is-active .xterm-rows > div').filter({ hasText: commandMarker }).waitFor();

  await page.screenshot({ path: join(outputDir, 'desktop.png'), fullPage: true });
  await page.setViewportSize({ width: 390, height: 844 });
  await page.waitForTimeout(250);
  const mobileOverflow = await page.evaluate(
    () => globalThis.document.documentElement.scrollWidth - globalThis.window.innerWidth,
  );
  if (mobileOverflow > 1) throw new Error(`mobile viewport overflows horizontally by ${mobileOverflow}px`);
  const mobileTerminal = await page.locator('.terminal-view.is-active').boundingBox();
  if (!mobileTerminal || mobileTerminal.width < 300 || mobileTerminal.height < 500) {
    throw new Error(`mobile terminal collapsed: ${JSON.stringify(mobileTerminal)}`);
  }
  await page.getByRole('button', { name: '打开侧栏' }).click();
  const mobileClose = page.getByRole('button', { name: '关闭侧栏' });
  await mobileClose.waitFor();
  if (!(await mobileClose.isVisible()) || (await page.getByRole('button', { name: '收起侧栏' }).isVisible())) {
    throw new Error('mobile sidebar did not exclusively expose its close action');
  }
  await mobileClose.click();
  await page.screenshot({ path: join(outputDir, 'mobile.png'), fullPage: true });

  if (ownedServer) {
    const pasteText = 'wmux-汉🙂\n'.repeat(12_000);
    const normalizedPaste = pasteText.replace(/\r?\n/g, '\r');
    const normalizedBytes = Buffer.from(normalizedPaste);
    pasteBytes = normalizedBytes.length;
    if (pasteBytes <= 128 * 1024) throw new Error(`large-paste fixture is only ${pasteBytes} bytes`);
    const pasteHash = createHash('sha256').update(normalizedBytes).digest('hex');
    const pasteProbe = String.raw`
import hashlib, os, select, termios, time, tty
old = termios.tcgetattr(0)
try:
    tty.setraw(0)
    os.write(1, b'\x1b[?2004h\r\nPASTE_ARMED\r\n')
    expected_body = ${pasteBytes}
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
`;
    await terminalInput.focus();
    await page.keyboard.insertText(encodedPythonCommand(pasteProbe));
    await page.keyboard.press('Enter');
    await page.locator('.terminal-view.is-active .xterm-rows').filter({ hasText: 'PASTE_ARMED' }).waitFor();
    await page.evaluate((text) => globalThis.navigator.clipboard.writeText(text), pasteText);
    await page.getByRole('button', { name: '粘贴', exact: true }).click();
    const pasteResult = page.locator('.terminal-view.is-active .xterm-rows').filter({ hasText: /PASTE_(?:OK|BAD)/ });
    await pasteResult.waitFor({ timeout: 20_000 });
    if (!(await pasteResult.textContent())?.includes('PASTE_OK')) {
      throw new Error(`large bracketed paste failed: ${await pasteResult.textContent()}`);
    }

    const replayProbe = String.raw`
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
`;
    await terminalInput.focus();
    await page.keyboard.insertText(encodedPythonCommand(replayProbe));
    await page.keyboard.press('Enter');
    const replayArmed = page
      .locator('.terminal-view.is-active .xterm-rows')
      .filter({ hasText: /REPLAY_(?:ARMED|SETUP_FAILED)/ });
    await replayArmed.waitFor({ timeout: 10_000 });
    if ((await replayArmed.textContent())?.includes('REPLAY_SETUP_FAILED')) {
      throw new Error('xterm did not answer the live CSI 5n setup query');
    }

    await page.reload({ waitUntil: 'domcontentloaded' });
    await page.locator('.terminal-view.is-active[data-replay-complete="true"]').waitFor({ timeout: 20_000 });
    const replayResult = page
      .locator('.terminal-view.is-active .xterm-rows')
      .filter({ hasText: /REPLAY_(?:SAFE|LEAK)/ });
    await replayResult.waitFor({ timeout: 20_000 });
    const replayText = await replayResult.textContent();
    if (!replayText?.includes('REPLAY_SAFE') || replayText.includes('REPLAY_LEAK')) {
      throw new Error(`historical terminal query leaked input after reload: ${replayText}`);
    }
    replayIsolation = 'REPLAY_SAFE';
  }

  await page.getByRole('button', { name: /结束会话 浏览器验收/ }).click();
  const confirmDialog = page.getByRole('dialog', { name: /结束「浏览器验收」/ });
  await confirmDialog.waitFor();
  if ((await confirmDialog.getByText('将结束进程并删除终端历史。', { exact: true }).count()) !== 1) {
    throw new Error('session confirmation does not contain one concrete consequence');
  }
  if (await confirmDialog.getByText('这个操作无法撤销。', { exact: true }).count()) {
    throw new Error('session confirmation still contains the generic duplicate warning');
  }
  const deletedResponse = page.waitForResponse(
    (response) => response.request().method() === 'DELETE' && response.url().includes('/api/sessions/'),
  );
  await confirmDialog.getByRole('button', { name: '结束会话' }).click();
  const deleted = await deletedResponse;
  if (deleted.status() !== 204) throw new Error(`UI session termination returned ${deleted.status()}`);

  const remaining = await page.evaluate(async () => {
    const response = await fetch('/api/sessions', { credentials: 'same-origin' });
    return response.json();
  });
  if (!Array.isArray(remaining) || remaining.length) {
    throw new Error(`UI session termination left sessions behind: ${JSON.stringify(remaining)}`);
  }
  if (browserErrors.length) throw new Error(browserErrors.join('\n'));

  process.stdout.write(
    `${JSON.stringify(
      {
        ok: true,
        commandOutput: commandMarker,
        screenshots: outputDir,
        chrome: executablePath,
        version: buildStatus.version,
        commit: buildStatus.commit,
        mobileOverflow,
        mobileTerminal,
        terminalFontRequests: delayedTerminalFonts.length,
        pasteBytes,
        replayIsolation,
        sshConfigImport,
        uiTerminationStatus: deleted.status(),
        remainingSessions: remaining.length,
      },
      null,
      2,
    )}\n`,
  );
} finally {
  if (browser) await browser.close();
  if (ownedServer) await stopChild(ownedServer);
  if (ownedDataDir) await rm(ownedDataDir, { recursive: true, force: true });
}

async function findChrome() {
  const candidates = [
    process.env.CHROME_PATH,
    '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome',
    '/Applications/Chromium.app/Contents/MacOS/Chromium',
    '/usr/bin/google-chrome',
    '/usr/bin/google-chrome-stable',
    '/usr/bin/chromium',
    '/usr/bin/chromium-browser',
  ].filter(Boolean);
  for (const candidate of candidates) {
    try {
      await access(candidate);
      return candidate;
    } catch {
      // Try the next common installation path.
    }
  }
  throw new Error('Chrome/Chromium was not found; set CHROME_PATH to its executable');
}

function availablePort() {
  return new Promise((resolvePort, reject) => {
    const server = createServer();
    server.unref();
    server.once('error', reject);
    server.listen(0, '127.0.0.1', () => {
      const address = server.address();
      const port = typeof address === 'object' && address ? address.port : 0;
      server.close((error) => (error ? reject(error) : resolvePort(port)));
    });
  });
}

async function waitForHealth(url, child) {
  const deadline = Date.now() + 10_000;
  while (Date.now() < deadline) {
    if (child.exitCode !== null) throw new Error(`wmux exited before becoming ready (${child.exitCode})`);
    try {
      const response = await fetch(url);
      if (response.ok) return;
    } catch {
      // Startup is still in progress.
    }
    await new Promise((resolveWait) => setTimeout(resolveWait, 100));
  }
  throw new Error(`wmux did not become healthy at ${url}`);
}

async function stopChild(child) {
  if (child.exitCode !== null) return;
  child.kill('SIGTERM');
  const exited = new Promise((resolveExit) => child.once('exit', resolveExit));
  const timedOut = new Promise((resolveTimeout) => setTimeout(() => resolveTimeout('timeout'), 5_000));
  if ((await Promise.race([exited, timedOut])) === 'timeout' && child.exitCode === null) {
    child.kill('SIGKILL');
    await exited;
  }
}

function encodedPythonCommand(source) {
  const encoded = Buffer.from(source).toString('base64');
  return `python3 -c "import base64;exec(base64.b64decode('${encoded}'))"`;
}
