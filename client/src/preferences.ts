import type { TerminalPreferences } from './types';

export const DEFAULT_PREFERENCES: TerminalPreferences = {
  fontSize: 14,
  cursorStyle: 'block',
  cursorBlink: true,
  scrollback: 10000,
  theme: 'light',
};

export function isMobileLayout(): boolean {
  return getComputedStyle(document.documentElement).getPropertyValue('--mobile-layout').trim() === '1';
}
