/**
 * features/ui/RowMenu.tsx -- the "…" actions menu on a table row.
 *
 * Replaces the row-action half of shared/components/list-page/ListTable.tsx.
 *
 * The template covers no such control -- its tables were read-only -- so the
 * chrome is borrowed from the one menu it does have, the sidebar settings
 * pop-up (example.html:89-102): a glass panel, rounded, shadowed, sitting above
 * its trigger. Dismissal follows features/layout/SettingsMenu.tsx exactly:
 * outside pointerdown and Escape, not a document click listener fighting a
 * stopPropagation.
 *
 * `danger` items paint with --color-error rather than a literal red, so a
 * destructive action looks the same here as it does anywhere else in the app.
 */

import React, { useEffect, useRef, useState } from 'react';
import { FiMoreHorizontal } from '@/features/ui/icons';

export interface RowMenuItem {
  id: string;
  label: string;
  icon?: React.ReactNode;
  variant?: 'default' | 'danger';
  onClick: () => void;
}

export interface RowMenuProps {
  items: RowMenuItem[];
  /** Named for the row it belongs to, so every trigger is distinguishable. */
  label: string;
}

export const RowMenu: React.FC<RowMenuProps> = ({ items, label }) => {
  const [open, setOpen] = useState(false);
  const wrapRef = useRef<HTMLDivElement>(null);

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

  if (items.length === 0) return null;

  return (
    <div ref={wrapRef} className="relative flex justify-end">
      <button
        type="button"
        onClick={() => setOpen((v) => !v)}
        aria-haspopup="menu"
        aria-expanded={open}
        aria-label={label}
        className="rounded p-1.5 text-slate-500 transition-colors hover:bg-slate-200/60 hover:text-slate-900 dark:text-slate-400 dark:hover:bg-slate-700/40 dark:hover:text-slate-100"
      >
        <FiMoreHorizontal className="h-4 w-4" />
      </button>
      {open && (
        <div
          role="menu"
          className="admin-glass absolute right-0 top-full z-20 mt-1 min-w-44 rounded-lg p-1 shadow-lg"
        >
          {items.map((item) => (
            <button
              key={item.id}
              role="menuitem"
              type="button"
              onClick={() => {
                setOpen(false);
                item.onClick();
              }}
              className="flex w-full items-center gap-2 rounded-md px-3 py-2 text-left text-sm transition-colors hover:bg-slate-200/60 dark:hover:bg-slate-700/40"
              style={item.variant === 'danger' ? { color: 'var(--color-error)' } : undefined}
            >
              {item.icon ? <span aria-hidden="true">{item.icon}</span> : null}
              {item.label}
            </button>
          ))}
        </div>
      )}
    </div>
  );
};

export default RowMenu;
