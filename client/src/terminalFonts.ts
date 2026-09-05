import { loadFonts } from '@xterm/addon-web-fonts';

export type TerminalFontId =
  | 'jetbrains-mono'
  | 'fira-code'
  | 'cascadia-code'
  | 'source-code-pro'
  | 'roboto-mono'
  | 'ibm-plex-mono'
  | 'ubuntu-mono'
  | 'system';

export type TerminalFont = {
  id: TerminalFontId;
  label: string;
  /** Family name declared by the bundled @font-face rules; null for the platform stack. */
  family: string | null;
  /**
   * Injects the @font-face rules for this family. The font files themselves only
   * download once xterm (or a preview) first uses the family.
   */
  load: () => Promise<unknown>;
};

const NERD_SYMBOLS_FAMILY = 'Symbols Nerd Font Mono';
const PLATFORM_MONOSPACE = ['ui-monospace', '"SFMono-Regular"', 'Menlo', 'Consolas', '"Liberation Mono"'];

/** Fallback stack used before any webfont resolves and behind every loaded family. */
export const TERMINAL_SYSTEM_FONT_FAMILY = [...PLATFORM_MONOSPACE, 'monospace'].join(', ');

export const DEFAULT_TERMINAL_FONT: TerminalFontId = 'jetbrains-mono';

// JetBrains Mono ships with the application shell (see main.tsx); every other
// family is a separate chunk that is only fetched when the user selects it.
export const TERMINAL_FONTS: readonly TerminalFont[] = [
  { id: 'jetbrains-mono', label: 'JetBrains Mono', family: 'JetBrains Mono Variable', load: () => Promise.resolve() },
  {
    id: 'fira-code',
    label: 'Fira Code',
    family: 'Fira Code Variable',
    load: () => import('@fontsource-variable/fira-code'),
  },
  {
    id: 'cascadia-code',
    label: 'Cascadia Code',
    family: 'Cascadia Code Variable',
    load: () =>
      Promise.all([
        import('@fontsource-variable/cascadia-code'),
        import('@fontsource-variable/cascadia-code/wght-italic.css'),
      ]),
  },
  {
    id: 'source-code-pro',
    label: 'Source Code Pro',
    family: 'Source Code Pro Variable',
    load: () =>
      Promise.all([
        import('@fontsource-variable/source-code-pro'),
        import('@fontsource-variable/source-code-pro/wght-italic.css'),
      ]),
  },
  {
    id: 'roboto-mono',
    label: 'Roboto Mono',
    family: 'Roboto Mono Variable',
    load: () =>
      Promise.all([
        import('@fontsource-variable/roboto-mono'),
        import('@fontsource-variable/roboto-mono/wght-italic.css'),
      ]),
  },
  {
    id: 'ibm-plex-mono',
    label: 'IBM Plex Mono',
    family: 'IBM Plex Mono',
    // Static family: regular, the 600 weight xterm uses for bold, and italic.
    load: () =>
      Promise.all([
        import('@fontsource/ibm-plex-mono'),
        import('@fontsource/ibm-plex-mono/600.css'),
        import('@fontsource/ibm-plex-mono/400-italic.css'),
      ]),
  },
  {
    id: 'ubuntu-mono',
    label: 'Ubuntu Mono',
    family: 'Ubuntu Mono',
    // Static family with only 400 and 700; the browser maps xterm's 600 to 700.
    load: () =>
      Promise.all([
        import('@fontsource/ubuntu-mono'),
        import('@fontsource/ubuntu-mono/700.css'),
        import('@fontsource/ubuntu-mono/400-italic.css'),
      ]),
  },
  { id: 'system', label: '系统等宽字体', family: null, load: () => Promise.resolve() },
];

export const TERMINAL_FONT_IDS: readonly TerminalFontId[] = TERMINAL_FONTS.map((font) => font.id);

export function terminalFont(id: TerminalFontId): TerminalFont {
  return TERMINAL_FONTS.find((font) => font.id === id) ?? TERMINAL_FONTS[0]!;
}

/** The CSS stack for a font choice; `loaded` restricts the webfont families to the ones that resolved. */
export function terminalFontStack(id: TerminalFontId, loaded?: readonly string[]): string {
  const font = terminalFont(id);
  const families = font.family ? [font.family, NERD_SYMBOLS_FAMILY] : [];
  const usable = loaded ? families.filter((family) => loaded.includes(family)) : families;
  const stack = font.family
    ? [...usable.map((family) => `"${family}"`), ...PLATFORM_MONOSPACE]
    : [...PLATFORM_MONOSPACE, ...(loaded && !loaded.includes(NERD_SYMBOLS_FAMILY) ? [] : [`"${NERD_SYMBOLS_FAMILY}"`])];
  return [...stack, 'monospace'].join(', ');
}

/**
 * Resolves the stack xterm should open with: the chosen webfont and the Nerd
 * symbol fallback are loaded independently so one failed asset still leaves
 * the others usable.
 */
export async function resolveTerminalFontFamily(id: TerminalFontId = DEFAULT_TERMINAL_FONT): Promise<string> {
  const font = terminalFont(id);
  try {
    await font.load();
  } catch {
    // Without its @font-face rules the family is simply not registered and drops out below.
  }
  const families = font.family ? [font.family, NERD_SYMBOLS_FAMILY] : [NERD_SYMBOLS_FAMILY];
  const results = await Promise.allSettled(
    families.map(async (family) => {
      await loadFonts([family]);
      return family;
    }),
  );
  const loaded = results.flatMap((result) => (result.status === 'fulfilled' ? [result.value] : []));
  return terminalFontStack(id, loaded);
}
