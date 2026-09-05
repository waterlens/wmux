import { describe, expect, it, vi } from 'vitest';
import { resolveTerminalFontFamily, TERMINAL_SYSTEM_FONT_FAMILY } from './terminalFonts';

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
});
