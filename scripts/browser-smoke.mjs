import { spawn } from 'node:child_process';
import { access, mkdir, mkdtemp, rm } from 'node:fs/promises';
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
let ownedServer;
let ownedDataDir;
let baseURL = process.env.WMUX_BROWSER_URL;
let buildStatus;
const executablePath = await findChrome();
let browser;

try {
  if (!baseURL) {
    const binary = resolve(process.env.WMUX_BROWSER_BINARY ?? join(projectDir, 'dist', 'wmux'));
    await access(binary).catch(() => {
      throw new Error(`wmux binary not found at ${binary}; run pnpm build first`);
    });
    const port = await availablePort();
    baseURL = `http://127.0.0.1:${port}`;
    ownedDataDir = await mkdtemp(join(tmpdir(), 'wmux-browser-data-'));
    ownedServer = spawn(binary, [], {
      cwd: projectDir,
      env: {
        ...process.env,
        WMUX_HOST: '127.0.0.1',
        WMUX_PORT: String(port),
        WMUX_DATA_DIR: ownedDataDir,
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
  const page = await browser.newPage({ viewport: { width: 1440, height: 960 } });

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

  await page.getByRole('button', { name: /管理 SSH 主机/ }).click();
  await page.getByRole('heading', { name: 'SSH 主机' }).waitFor();
  await page.screenshot({ path: join(outputDir, 'hosts.png'), fullPage: true });

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
  await dialog.getByLabel('会话名称').fill('浏览器验收');
  await dialog.getByLabel('持久化方式').selectOption('none');
  await dialog.getByRole('button', { name: '启动会话' }).click();

  await page.locator('.terminal-view.is-active .xterm-helper-textarea').waitFor();
  await page.getByText('已连接', { exact: true }).waitFor();
  const terminalInput = page.locator('.terminal-view.is-active .xterm-helper-textarea');
  await terminalInput.focus();
  await page.keyboard.type("printf 'WMUX_BROWSER_OK\\n'", { delay: 10 });
  await page.keyboard.press('Enter');
  await page.locator('.terminal-view.is-active .xterm-rows').filter({ hasText: 'WMUX_BROWSER_OK' }).waitFor();

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
  await page.screenshot({ path: join(outputDir, 'mobile.png'), fullPage: true });

  await page.getByRole('button', { name: /结束会话 浏览器验收/ }).click();
  const confirmDialog = page.getByRole('dialog', { name: /结束「浏览器验收」/ });
  await confirmDialog.waitFor();
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
        commandOutput: 'WMUX_BROWSER_OK',
        screenshots: outputDir,
        chrome: executablePath,
        version: buildStatus.version,
        commit: buildStatus.commit,
        mobileOverflow,
        mobileTerminal,
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
