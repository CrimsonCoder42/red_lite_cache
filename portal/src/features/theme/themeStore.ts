/**
 * features/theme/themeStore.ts -- the one writer of the theme class.
 *
 * Adapted from the template's inline theme code (example.js:9-34), with two
 * corrections:
 *
 * 1. ONE CLASS SYSTEM. The template toggled `html.dark` / `html.light` AND
 *    `body.dark-theme` / `body.light-theme` on every change, and its CSS keyed
 *    off both -- `.dark .glass-ui` (example.css:138) against
 *    `body.dark-theme` (example.css:16). Two switches for one state is two
 *    things to forget. Only the `html` classes survive: Tailwind's dark variant
 *    reads them (index.css:225) and so do the fork's existing
 *    `.dark .modal-*` rules (index.css:544 onward).
 *
 * 2. NO BACKEND WRITE. `shared/context/ThemeContext.tsx:99-107` fires
 *    `preferencesService.updateTheme()` on every change. This module persists to
 *    localStorage only -- the behaviour the template had, and the only behaviour
 *    the admin shell needs. Theme is a per-browser preference here; syncing it
 *    to the account is a separate decision that has not been made.
 *
 * The storage key and the class names are duplicated in exactly one other
 * place: the pre-paint guard in index.html, which has to be inline to beat the
 * first paint and therefore cannot import this file. Change one, change both.
 */

export type ThemeMode = 'light' | 'dark' | 'auto';

/** Same key the fork already uses (shared/context/ThemeContext.tsx:29). */
const STORAGE_KEY = 'theme';

type Listener = (mode: ThemeMode) => void;
const listeners = new Set<Listener>();

/** localStorage throws outright in some privacy modes, not just return null. */
function safeRead(): ThemeMode | null {
  try {
    const raw = localStorage.getItem(STORAGE_KEY);
    return raw === 'light' || raw === 'dark' || raw === 'auto' ? raw : null;
  } catch {
    return null;
  }
}

function safeWrite(mode: ThemeMode): void {
  try {
    localStorage.setItem(STORAGE_KEY, mode);
  } catch {
    /* Nothing to do -- the class is already applied, it just will not survive a reload. */
  }
}

export function systemPrefersDark(): boolean {
  if (typeof window === 'undefined' || !window.matchMedia) return false;
  return window.matchMedia('(prefers-color-scheme: dark)').matches;
}

/** The stored mode, or 'auto' when nothing has been chosen on this browser. */
export function getThemeMode(): ThemeMode {
  return safeRead() ?? 'auto';
}

/** A mode plus the OS setting resolves to the one thing the DOM cares about. */
export function resolveIsDark(mode: ThemeMode = getThemeMode()): boolean {
  return mode === 'auto' ? systemPrefersDark() : mode === 'dark';
}

/**
 * Writes the class pair. The ONLY place in the package that does -- including
 * ThemeContext, which delegates here (shared/context/ThemeContext.tsx).
 */
export function applyThemeClass(isDark: boolean): void {
  if (typeof document === 'undefined') return;
  const root = document.documentElement;
  root.classList.toggle('dark', isDark);
  root.classList.toggle('light', !isDark);
}

export function setThemeMode(mode: ThemeMode): void {
  safeWrite(mode);
  applyThemeClass(resolveIsDark(mode));
  listeners.forEach((fn) => fn(mode));
}

/** Explicit light ⇄ dark. 'auto' resolves first, so the tap always visibly flips. */
export function toggleThemeMode(): ThemeMode {
  const next: ThemeMode = resolveIsDark() ? 'light' : 'dark';
  setThemeMode(next);
  return next;
}

export function subscribeTheme(fn: Listener): () => void {
  listeners.add(fn);
  return () => {
    listeners.delete(fn);
  };
}

/**
 * Re-resolves on an OS change, but only while the mode is 'auto'. A person who
 * explicitly chose light does not want the OS overruling them at sunset.
 */
export function watchSystemTheme(): () => void {
  if (typeof window === 'undefined' || !window.matchMedia) return () => {};
  const mq = window.matchMedia('(prefers-color-scheme: dark)');
  const onChange = (): void => {
    if (getThemeMode() === 'auto') {
      applyThemeClass(resolveIsDark('auto'));
      listeners.forEach((fn) => fn('auto'));
    }
  };
  mq.addEventListener('change', onChange);
  return () => mq.removeEventListener('change', onChange);
}
