import type { TerminalPreferences } from './types';

const STORAGE_KEY = 'wmux.terminalPreferences';

export const DEFAULT_PREFERENCES: TerminalPreferences = {
  fontSize: 14,
  cursorStyle: 'block',
  cursorBlink: true,
  scrollback: 10_000,
  theme: 'light',
};

export const FONT_SIZE_RANGE = { min: 11, max: 22 };

export const SCROLLBACK_OPTIONS: { value: number; label: string }[] = [
  { value: 2_000, label: '2,000 行' },
  { value: 10_000, label: '10,000 行' },
  { value: 25_000, label: '25,000 行' },
  { value: 50_000, label: '50,000 行' },
];

const CURSOR_STYLES: TerminalPreferences['cursorStyle'][] = ['block', 'bar', 'underline'];
const THEMES: TerminalPreferences['theme'][] = ['light', 'dark', 'system'];

function allowed<T>(values: readonly T[], stored: unknown, fallback: T): T {
  return values.includes(stored as T) ? (stored as T) : fallback;
}

export function loadPreferences(): TerminalPreferences {
  try {
    const stored = JSON.parse(localStorage.getItem(STORAGE_KEY) ?? '{}') as Partial<TerminalPreferences>;
    return {
      fontSize:
        typeof stored.fontSize === 'number'
          ? Math.min(FONT_SIZE_RANGE.max, Math.max(FONT_SIZE_RANGE.min, stored.fontSize))
          : DEFAULT_PREFERENCES.fontSize,
      cursorStyle: allowed(CURSOR_STYLES, stored.cursorStyle, DEFAULT_PREFERENCES.cursorStyle),
      cursorBlink: typeof stored.cursorBlink === 'boolean' ? stored.cursorBlink : DEFAULT_PREFERENCES.cursorBlink,
      scrollback: allowed(
        SCROLLBACK_OPTIONS.map((option) => option.value),
        stored.scrollback,
        DEFAULT_PREFERENCES.scrollback,
      ),
      theme: allowed(THEMES, stored.theme, DEFAULT_PREFERENCES.theme),
    };
  } catch {
    return DEFAULT_PREFERENCES;
  }
}

export function savePreferences(preferences: TerminalPreferences): void {
  localStorage.setItem(STORAGE_KEY, JSON.stringify(preferences));
}
