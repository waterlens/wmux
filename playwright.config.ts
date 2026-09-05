import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { defineConfig } from '@playwright/test';

// Playwright reloads this file in every worker, so the port and the data
// directory have to be constants rather than per-process values.
const port = 8788;
const baseURL = `http://127.0.0.1:${port}`;

/** Throwaway wmux state: recreated on every run, removed by the global teardown. */
export const dataDir = join(tmpdir(), 'wmux-browser-test');

export const desktopViewport = { width: 1440, height: 960 };
export const mobileViewport = { width: 390, height: 844 };

export default defineConfig({
  testDir: 'tests/browser',
  globalTeardown: './tests/browser/teardown.ts',
  // The tests share one wmux instance and one admin account.
  fullyParallel: false,
  workers: 1,
  forbidOnly: Boolean(process.env.CI),
  reporter: 'list',
  timeout: 90_000,
  expect: { timeout: 15_000 },
  use: {
    baseURL,
    viewport: desktopViewport,
    permissions: ['clipboard-read', 'clipboard-write'],
    screenshot: 'only-on-failure',
    trace: 'retain-on-failure',
    // The tests read terminal text from the DOM renderer's rows. The WebGL
    // renderer paints to a canvas instead, so disable WebGL and exercise the
    // documented DOM fallback path.
    launchOptions: { args: ['--disable-webgl'] },
  },
  webServer: {
    command: `rm -rf '${dataDir}' && ./bin/wmux`,
    url: `${baseURL}/api/health`,
    reuseExistingServer: false,
    stdout: 'pipe',
    stderr: 'pipe',
    env: {
      WMUX_HOST: '127.0.0.1',
      WMUX_PORT: String(port),
      WMUX_DATA_DIR: dataDir,
      WMUX_PUBLIC_URL: baseURL,
      WMUX_LOG_LEVEL: 'warn',
    },
  },
});
