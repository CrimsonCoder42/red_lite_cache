/**
 * features/theme/useAdminTheme.ts -- React's view of themeStore.
 *
 * The store is the source of truth and subscription is external, so any number
 * of components can read the theme without one of them owning it. The shell's
 * ThemeToggle and the sidebar's settings menu both use this and stay in step.
 */

import { useCallback, useEffect, useState } from 'react';
import {
  getThemeMode,
  resolveIsDark,
  setThemeMode,
  subscribeTheme,
  toggleThemeMode,
  watchSystemTheme,
  type ThemeMode,
} from './themeStore';

export interface AdminTheme {
  mode: ThemeMode;
  isDark: boolean;
  setMode: (mode: ThemeMode) => void;
  toggle: () => void;
}

export function useAdminTheme(): AdminTheme {
  const [mode, setMode] = useState<ThemeMode>(getThemeMode);
  const [isDark, setIsDark] = useState<boolean>(() => resolveIsDark());

  useEffect(() => {
    const sync = (next: ThemeMode): void => {
      setMode(next);
      setIsDark(resolveIsDark(next));
    };
    // The pre-paint guard (index.html) already wrote the class; this re-reads it
    // so React's state starts out agreeing with the DOM rather than guessing.
    sync(getThemeMode());
    const unsubscribe = subscribeTheme(sync);
    const unwatch = watchSystemTheme();
    return () => {
      unsubscribe();
      unwatch();
    };
  }, []);

  return {
    mode,
    isDark,
    setMode: useCallback((next: ThemeMode) => setThemeMode(next), []),
    toggle: useCallback(() => {
      toggleThemeMode();
    }, []),
  };
}
