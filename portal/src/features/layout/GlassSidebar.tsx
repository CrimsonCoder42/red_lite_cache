/**
 * features/layout/GlassSidebar.tsx -- the glass sidebar, copied from the reference admin.
 *
 * Trimmed: the user and the role come from the auth context (not Redux); the brand reads
 * red_lite_cache; the reference's single-level nav groups are not needed (every item is a link);
 * the settings menu's "Back to App" link becomes "Change password" (the voluntary /password page).
 */

import React from 'react';
import { Link } from 'react-router-dom';
import { FiActivity, FiLogOut, FiSettings } from '@/features/ui/icons';
import { useAuth } from '@/auth/AuthContext';
import { useToast } from '@/features/ui/Toast';
import { visibleNav } from './adminNav';
import SidebarNavLink from './SidebarNavLink';
import SettingsMenu from './SettingsMenu';

export interface GlassSidebarProps {
  open: boolean;
  /** Called after any navigation, so a narrow viewport closes the drawer. */
  onNavigate: () => void;
}

export const GlassSidebar: React.FC<GlassSidebarProps> = ({ open, onNavigate }) => {
  const { me, logout } = useAuth();
  const toast = useToast();
  const items = visibleNav(me?.role);
  const displayName = me?.user?.name || me?.user?.email || me?.actor || 'Admin';

  return (
    <aside
      data-testid="admin-sidebar"
      aria-label="Admin navigation"
      className={[
        'admin-glass fixed inset-y-0 left-0 z-40 flex w-64 flex-col',
        'transition-transform duration-300 ease-in-out',
        open ? 'translate-x-0' : '-translate-x-full',
      ].join(' ')}
    >
      <div className="flex h-16 items-center px-4">
        <FiActivity className="mr-2 h-8 w-8 text-indigo-500 dark:text-indigo-400" aria-hidden="true" />
        <span className="text-xl font-bold text-slate-800 dark:text-slate-200">red_lite_cache</span>
        <span className="ml-2 rounded bg-purple-100 px-2 py-0.5 text-xs font-medium text-purple-700 dark:bg-purple-900/30 dark:text-purple-300">
          Admin
        </span>
      </div>

      <nav className="flex-1 space-y-1 overflow-y-auto px-4 py-6" aria-label="Sections">
        {items.map((item) => (
          <SidebarNavLink key={item.key} to={item.path} label={item.label} icon={item.icon} onNavigate={onNavigate} />
        ))}
      </nav>

      <SettingsMenu>
        <p className="truncate px-3 pt-2 text-xs text-slate-500 dark:text-slate-400">
          {displayName} ({me?.role})
        </p>
        <Link
          to="/password"
          className="flex items-center gap-2 rounded-md px-3 py-2 text-sm text-slate-600 transition-colors hover:bg-slate-100/60 dark:text-slate-300 dark:hover:bg-slate-600/40"
        >
          <FiSettings className="h-4 w-4" />
          <span>Change password</span>
        </Link>
        <button
          type="button"
          onClick={() => logout().catch(toast.fail)}
          className="flex w-full items-center gap-2 rounded-md px-3 py-2 text-sm text-red-600 transition-colors hover:bg-red-50 dark:text-red-400 dark:hover:bg-red-900/20"
        >
          <FiLogOut className="h-4 w-4" />
          <span>Sign Out</span>
        </button>
      </SettingsMenu>
    </aside>
  );
};

export default GlassSidebar;
