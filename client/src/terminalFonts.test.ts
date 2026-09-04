import { describe, expect, it, vi } from 'vitest';
import { openTerminalAfterFonts, resolveTerminalFontFamily, TERMINAL_SYSTEM_FONT_FAMILY } from './terminalFonts';

describe('terminal webfont initialization', () => {
  it('does not open or fit xterm while the font response is delayed', async () => {
    let releaseFont!: (family: string) => void;
    const delayedFont = new Promise<string>((resolve) => {
      releaseFont = resolve;
    });
    const calls: string[] = [];

    const initializing = openTerminalAfterFonts(
      (family) => calls.push(`open:${family}`),
      () => calls.push('fit'),
      () => false,
      () => delayedFont,
    );

    await Promise.resolve();
    expect(calls).toEqual([]);
    releaseFont('"JetBrains Mono Variable", monospace');
    await expect(initializing).resolves.toBe(true);
    expect(calls).toEqual(['open:"JetBrains Mono Variable", monospace', 'fit']);
  });

  it('falls back per family when an optional webfont cannot load', async () => {
    const loader = vi.fn(async (families: (string | FontFace)[]) => {
      if (families[0] === 'Symbols Nerd Font Mono') throw new Error('missing font face');
      return [];
    });

    const family = await resolveTerminalFontFamily(loader);
    expect(loader).toHaveBeenCalledTimes(2);
    expect(family).toContain('"JetBrains Mono Variable"');
    expect(family).not.toContain('"Symbols Nerd Font Mono"');
    expect(family).toContain(TERMINAL_SYSTEM_FONT_FAMILY);
  });

  it('does not mount a terminal whose React effect was cleaned up while fonts loaded', async () => {
    const open = vi.fn();
    const fit = vi.fn();
    await expect(
      openTerminalAfterFonts(
        open,
        fit,
        () => true,
        async () => 'monospace',
      ),
    ).resolves.toBe(false);
    expect(open).not.toHaveBeenCalled();
    expect(fit).not.toHaveBeenCalled();
  });
});
