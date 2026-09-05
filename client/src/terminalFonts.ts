import { loadFonts } from '@xterm/addon-web-fonts';

const TERMINAL_WEB_FONT_FAMILIES = ['JetBrains Mono Variable', 'Symbols Nerd Font Mono'] as const;
export const TERMINAL_SYSTEM_FONT_FAMILY = '"SFMono-Regular", Consolas, "Liberation Mono", monospace';

/** Load each family independently so one failed asset still leaves the others usable. */
export async function resolveTerminalFontFamily(): Promise<string> {
  const results = await Promise.allSettled(
    TERMINAL_WEB_FONT_FAMILIES.map(async (family) => {
      await loadFonts([family]);
      return family;
    }),
  );
  const loaded = results.flatMap((result) => (result.status === 'fulfilled' ? [`"${result.value}"`] : []));
  return [...loaded, TERMINAL_SYSTEM_FONT_FAMILY].join(', ');
}
