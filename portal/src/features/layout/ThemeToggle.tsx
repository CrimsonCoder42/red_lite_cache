/**
 * features/layout/ThemeToggle.tsx -- the light ⇄ dark switch.
 *
 * Adapted from the template's settings-menu row (example.html:91-102,
 * example.js:29-34). The sun and moon paths are the template's own; what changed
 * is that only ONE of them is rendered rather than both with `hidden` toggled by
 * hand, and the button carries an accessible name so a spec can find it by role.
 */

import React from 'react';
import { useAdminTheme } from '@/features/theme/useAdminTheme';

const SunIcon = (): React.ReactElement => (
  // example.html:94-96
  <svg
    className="h-5 w-5"
    viewBox="0 0 20 20"
    fill="none"
    stroke="currentColor"
    strokeWidth="1.5"
    strokeLinecap="round"
    strokeLinejoin="round"
    aria-hidden="true"
  >
    <path d="M10 2.5V4.5M14.25 5.75L12.835 7.165M17.5 10H15.5M14.25 14.25L12.835 12.835M10 17.5V15.5M5.75 14.25L7.165 12.835M2.5 10H4.5M5.75 5.75L7.165 7.165M10 12.5C11.3807 12.5 12.5 11.3807 12.5 10C12.5 8.61929 11.3807 7.5 10 7.5C8.61929 7.5 7.5 8.61929 7.5 10C7.5 11.3807 8.61929 12.5 10 12.5Z" />
  </svg>
);

const MoonIcon = (): React.ReactElement => (
  // example.html:97-99
  <svg className="h-5 w-5" viewBox="0 0 20 20" fill="currentColor" aria-hidden="true">
    <path d="M17.293 13.293A8 8 0 016.707 2.707a8.001 8.001 0 1010.586 10.586z" />
  </svg>
);

export const ThemeToggle: React.FC = () => {
  const { isDark, toggle } = useAdminTheme();

  return (
    <button
      type="button"
      onClick={toggle}
      data-testid="theme-toggle"
      /* The name says what the button DOES, not what is currently on, so the
         label a screen reader announces is an instruction rather than a status. */
      aria-label={isDark ? 'Switch to light theme' : 'Switch to dark theme'}
      aria-pressed={isDark}
      className="flex w-full items-center justify-between rounded-md px-3 py-2 text-sm text-slate-600 transition-colors hover:bg-slate-100/60 dark:text-slate-300 dark:hover:bg-slate-600/40"
    >
      <span>Theme</span>
      <span className="flex items-center gap-2">
        <span className="text-xs text-slate-500 dark:text-slate-400">{isDark ? 'Dark' : 'Light'}</span>
        {isDark ? <SunIcon /> : <MoonIcon />}
      </span>
    </button>
  );
};

export default ThemeToggle;
