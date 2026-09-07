import { useSyncExternalStore } from 'react';

/** The breakpoint lives in styles.css and reaches JS through the --mobile-layout variable. */
export function isMobileLayout(): boolean {
  return getComputedStyle(document.documentElement).getPropertyValue('--mobile-layout').trim() === '1';
}

function subscribe(onChange: () => void): () => void {
  window.addEventListener('resize', onChange);
  return () => window.removeEventListener('resize', onChange);
}

/** isMobileLayout() as React state, re-evaluated whenever the window resizes. */
export function useMobileLayout(): boolean {
  return useSyncExternalStore(subscribe, isMobileLayout, () => false);
}
