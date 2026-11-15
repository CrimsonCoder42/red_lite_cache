/**
 * features/layout/Topbar.tsx -- the glass header bar.
 *
 * example.html:118-132: a 64px glass strip holding the sidebar toggle on the
 * left and actions on the right. The template's right side held Sign In and
 * Register, which a staff console reached only when already signed in; the page
 * title goes there instead, and the theme switch is reachable from the sidebar
 * footer where the template put it.
 */

import React from 'react';
import { FiMenu } from '@/features/ui/icons';

export interface TopbarProps {
  onToggleSidebar: () => void;
  sidebarOpen: boolean;
  title: string;
  /** Page-level actions. Nothing renders here by default. */
  actions?: React.ReactNode;
}

export const Topbar: React.FC<TopbarProps> = ({ onToggleSidebar, sidebarOpen, title, actions }) => (
  <header
    data-testid="admin-topbar"
    className="admin-glass flex h-16 shrink-0 items-center justify-between gap-4 border-x-0 px-4 lg:px-8"
  >
    <div className="flex min-w-0 items-center gap-4">
      <button
        type="button"
        onClick={onToggleSidebar}
        aria-label={sidebarOpen ? 'Hide navigation' : 'Show navigation'}
        aria-expanded={sidebarOpen}
        data-testid="sidebar-toggle"
        className="text-slate-500 transition-colors hover:text-slate-900 dark:text-slate-400 dark:hover:text-slate-200"
      >
        <FiMenu className="h-6 w-6" />
      </button>
      {/* The only h1 on the page. Every admin screen's own heading is an h2. */}
      <h1 className="truncate text-lg font-semibold text-slate-800 dark:text-slate-100">{title}</h1>
    </div>
    {actions ? <div className="flex items-center gap-2">{actions}</div> : null}
  </header>
);

export default Topbar;
