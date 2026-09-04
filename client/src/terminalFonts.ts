import { loadFonts } from '@xterm/addon-web-fonts';

export const TERMINAL_WEB_FONT_FAMILIES = ['JetBrains Mono Variable', 'Symbols Nerd Font Mono'] as const;
export const TERMINAL_SYSTEM_FONT_FAMILY = '"SFMono-Regular", Consolas, "Liberation Mono", monospace';

type FontLoader = (families: (string | FontFace)[]) => Promise<FontFace[]>;

/** Load each family independently so one failed asset still leaves the others usable. */
export async function resolveTerminalFontFamily(loader: FontLoader = loadFonts): Promise<string> {
  const results = await Promise.allSettled(
    TERMINAL_WEB_FONT_FAMILIES.map(async (family) => {
      await loader([family]);
      return family;
    }),
  );
  const loaded = results.flatMap((result) => (result.status === 'fulfilled' ? [`"${result.value}"`] : []));
  return [...loaded, TERMINAL_SYSTEM_FONT_FAMILY].join(', ');
}

/** Keep xterm.open/fit behind the font promise to avoid caching fallback metrics. */
export async function openTerminalAfterFonts(
  open: (fontFamily: string) => void,
  fit: () => void,
  isCancelled: () => boolean,
  loadFamily: () => Promise<string> = resolveTerminalFontFamily,
): Promise<boolean> {
  const fontFamily = await loadFamily();
  if (isCancelled()) return false;
  open(fontFamily);
  fit();
  return true;
}
