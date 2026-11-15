/**
 * features/layout/SidebarNavLink.tsx -- one row in the sidebar.
 *
 * The template drove its active state by hand: on every hashchange it added
 * five classes to the matching anchor and removed the same five from every other
 * one, then reached into each anchor's <svg> to colour it (example.js:119-143).
 * react-router's NavLink already knows which route is current, so the active
 * classes are declarative here and the icon is coloured by `group-[.active]`
 * rather than by a querySelector.
 *
 * The class values themselves are the template's, unchanged.
 */

import React from 'react';
import { NavLink } from 'react-router-dom';

const BASE =
  'group flex items-center gap-3 rounded-lg px-4 py-2 text-slate-600 transition-colors duration-200 ' +
  'hover:bg-slate-200/60 hover:text-slate-900 dark:text-slate-400 dark:hover:bg-slate-700/30 dark:hover:text-slate-200';

/* example.js:121-130 */
const ACTIVE = 'active bg-slate-200/80 font-medium text-slate-900 dark:bg-slate-700/50 dark:text-slate-200';

export interface SidebarNavLinkProps {
  to: string;
  label: string;
  icon: React.ReactNode;
  /** A child row is indented; the parent group header supplies the context. */
  nested?: boolean;
  onNavigate?: () => void;
}

export const SidebarNavLink: React.FC<SidebarNavLinkProps> = ({
  to,
  label,
  icon,
  nested = false,
  onNavigate,
}) => (
  <NavLink
    to={to}
    /* Dashboard is '/admin', a prefix of every other admin path, so it would
       stay lit on every page without this. */
    end={to === '/admin'}
    onClick={onNavigate}
    className={({ isActive }) =>
      [BASE, isActive ? ACTIVE : '', nested ? 'pl-9 text-sm' : ''].filter(Boolean).join(' ')
    }
  >
    <span className="text-slate-500 transition-colors group-[.active]:text-indigo-500 dark:text-slate-400 dark:group-[.active]:text-indigo-400">
      {icon}
    </span>
    <span className="truncate">{label}</span>
  </NavLink>
);

export default SidebarNavLink;
