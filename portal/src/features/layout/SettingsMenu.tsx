/**
 * features/layout/SettingsMenu.tsx -- the sidebar footer pop-menu.
 *
 * Template: example.html:88-111, opened by example.js:50-58. Its dismiss handler
 * was a document-level click listener plus a stopPropagation on the trigger,
 * which is the pattern that leaves a menu stuck open the moment a second one
 * appears. This closes on outside pointerdown and on Escape, and it closes when
 * a route changes -- the three ways a person actually leaves a menu.
 */

import React, { useEffect, useRef, useState } from 'react';
import { useLocation } from 'react-router-dom';
import { FiSettings } from '@/features/ui/icons';
import ThemeToggle from './ThemeToggle';

export interface SettingsMenuProps {
  /** Rendered under the theme row -- sign out, back to app. */
  children?: React.ReactNode;
}

export const SettingsMenu: React.FC<SettingsMenuProps> = ({ children }) => {
  const [open, setOpen] = useState(false);
  const wrapRef = useRef<HTMLDivElement>(null);
  const { pathname } = useLocation();

  useEffect(() => setOpen(false), [pathname]);

  useEffect(() => {
    if (!open) return;
    const onPointerDown = (e: PointerEvent): void => {
      if (!wrapRef.current?.contains(e.target as Node)) setOpen(false);
    };
    const onKeyDown = (e: KeyboardEvent): void => {
      if (e.key === 'Escape') setOpen(false);
    };
    document.addEventListener('pointerdown', onPointerDown);
    document.addEventListener('keydown', onKeyDown);
    return () => {
      document.removeEventListener('pointerdown', onPointerDown);
      document.removeEventListener('keydown', onKeyDown);
    };
  }, [open]);

  return (
    <div ref={wrapRef} className="relative px-4 py-6">
      {open && (
        <div className="absolute bottom-full left-0 mb-2 w-full px-4">
          <div className="admin-glass rounded-lg p-2 shadow-lg" role="menu">
            <ThemeToggle />
            {children}
          </div>
        </div>
      )}
      <button
        type="button"
        onClick={() => setOpen((v) => !v)}
        aria-expanded={open}
        aria-haspopup="menu"
        data-testid="settings-button"
        className="flex w-full items-center gap-3 rounded-lg px-4 py-2 text-slate-600 transition-colors duration-200 hover:bg-slate-200/60 hover:text-slate-900 dark:text-slate-400 dark:hover:bg-slate-700/30 dark:hover:text-slate-200"
      >
        <FiSettings className="h-5 w-5 shrink-0" />
        <span>Settings</span>
      </button>
    </div>
  );
};

export default SettingsMenu;
