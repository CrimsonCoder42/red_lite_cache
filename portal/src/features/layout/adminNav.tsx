/**
 * features/layout/adminNav.tsx -- the sidebar items as data, with the role each needs.
 *
 * Shape copied from the reference admin's adminNav.tsx (items as data, filtered by the viewer's role
 * before render); its items are replaced by this portal's pages. An item is listed only when its
 * page exists, and only to a role the server lets reach it (doc/specs/s11_admin_portal.md).
 */

import React from 'react';
import { FiFolder } from '@/features/ui/icons';
import type { Role } from '@/api/types';
import { atLeast } from '@/auth/AuthContext';

export interface AdminNavItem {
  key: string;
  label: string;
  icon: React.ReactNode;
  path: string;
  /** The lowest role that sees the item. */
  minRole: Role;
}

const ICON = 'h-5 w-5 shrink-0';

export const ADMIN_NAV: AdminNavItem[] = [
  { key: 'projects', label: 'Projects', icon: <FiFolder className={ICON} />, path: '/projects', minRole: 'viewer' },
];

/** The items this role may see. */
export function visibleNav(role: Role | undefined, items: AdminNavItem[] = ADMIN_NAV): AdminNavItem[] {
  return items.filter((item) => atLeast(role, item.minRole));
}

/** The topbar title for a pathname: the item whose path is the pathname or its prefix. */
export function navTitle(pathname: string, items: AdminNavItem[] = ADMIN_NAV): string {
  const hit = items
    .filter((i) => pathname === i.path || (i.path !== '/' && pathname.startsWith(i.path + '/')))
    .sort((a, b) => b.path.length - a.path.length)[0];
  if (hit) return hit.label;
  if (pathname === '/password') return 'Password';
  return 'red_lite_cache';
}
