import { describe, expect, it, vi } from 'vitest';
import {
  resolveTerminalFontFamily,
  TERMINAL_FONTS,
  terminalFontStack,
  TERMINAL_SYSTEM_FONT_FAMILY,
} from './terminalFonts';

const fontHarness = vi.hoisted(() => ({ loadFonts: vi.fn() }));

vi.mock('@xterm/addon-web-fonts', () => ({ loadFonts: fontHarness.loadFonts }));

describe('terminal webfont initialization', () => {
  it('falls back per family when an optional webfont cannot load', async () => {
    fontHarness.loadFonts.mockImplementation(async (families: (string | FontFace)[]) => {
      if (families[0] === 'Symbols Nerd Font Mono') throw new Error('missing font face');
      return [];
    });

    const family = await resolveTerminalFontFamily();
    expect(fontHarness.loadFonts).toHaveBeenCalledTimes(2);
    expect(family).toContain('"JetBrains Mono Variable"');
    expect(family).not.toContain('"Symbols Nerd Font Mono"');
    expect(family).toContain(TERMINAL_SYSTEM_FONT_FAMILY);
  });

  it('loads an alternative family on demand and puts it first in the stack', async () => {
    fontHarness.loadFonts.mockReset();
    fontHarness.loadFonts.mockResolvedValue([]);

    const family = await resolveTerminalFontFamily('fira-code');
    expect(fontHarness.loadFonts).toHaveBeenCalledWith(['Fira Code Variable']);
    expect(fontHarness.loadFonts).toHaveBeenCalledWith(['Symbols Nerd Font Mono']);
    expect(family.startsWith('"Fira Code Variable", "Symbols Nerd Font Mono"')).toBe(true);
  });

  it('uses the platform stack for the system option and only waits for the symbol font', async () => {
    fontHarness.loadFonts.mockReset();
    fontHarness.loadFonts.mockResolvedValue([]);

    const family = await resolveTerminalFontFamily('system');
    expect(fontHarness.loadFonts).toHaveBeenCalledTimes(1);
    expect(fontHarness.loadFonts).toHaveBeenCalledWith(['Symbols Nerd Font Mono']);
    expect(family.startsWith('ui-monospace')).toBe(true);
    expect(family).toContain('"Symbols Nerd Font Mono"');
    expect(family.endsWith('monospace')).toBe(true);
  });

  it('exposes a preview stack for every bundled font', () => {
    for (const font of TERMINAL_FONTS) {
      const stack = terminalFontStack(font.id);
      if (font.family) expect(stack.startsWith(`"${font.family}"`)).toBe(true);
      expect(stack.endsWith('monospace')).toBe(true);
    }
  });
});
