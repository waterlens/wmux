// @vitest-environment jsdom

import { afterEach, describe, expect, it } from 'vitest';
import {
  AUTO_COLUMNS,
  COLUMN_RANGE,
  DEFAULT_PREFERENCES,
  FONT_SIZE_RANGE,
  isValidColumns,
  loadPreferences,
  savePreferences,
} from './preferences';

afterEach(() => {
  localStorage.clear();
});

describe('terminal preferences persistence', () => {
  it('round-trips every field', () => {
    savePreferences({ ...DEFAULT_PREFERENCES, fontFamily: 'ubuntu-mono', fontSize: 16, columns: 132, theme: 'dark' });
    expect(loadPreferences()).toEqual({
      ...DEFAULT_PREFERENCES,
      fontFamily: 'ubuntu-mono',
      fontSize: 16,
      columns: 132,
      theme: 'dark',
    });
  });

  it('falls back to the defaults for unknown fonts and out-of-range widths', () => {
    localStorage.setItem(
      'wmux.terminalPreferences',
      JSON.stringify({ fontFamily: 'comic-sans', fontSize: 40, columns: 12, cursorStyle: 'beam' }),
    );
    const preferences = loadPreferences();
    expect(preferences.fontFamily).toBe(DEFAULT_PREFERENCES.fontFamily);
    expect(preferences.fontSize).toBe(FONT_SIZE_RANGE.max);
    expect(preferences.columns).toBe(AUTO_COLUMNS);
    expect(preferences.cursorStyle).toBe(DEFAULT_PREFERENCES.cursorStyle);
  });

  it('accepts automatic width and integers inside the column range only', () => {
    expect(isValidColumns(AUTO_COLUMNS)).toBe(true);
    expect(isValidColumns(COLUMN_RANGE.min)).toBe(true);
    expect(isValidColumns(COLUMN_RANGE.max)).toBe(true);
    expect(isValidColumns(COLUMN_RANGE.min - 1)).toBe(false);
    expect(isValidColumns(COLUMN_RANGE.max + 1)).toBe(false);
    expect(isValidColumns(80.5)).toBe(false);
    expect(isValidColumns('80')).toBe(false);
  });

  it('returns the defaults when the stored value is not JSON', () => {
    localStorage.setItem('wmux.terminalPreferences', '{not json');
    expect(loadPreferences()).toEqual(DEFAULT_PREFERENCES);
  });
});
