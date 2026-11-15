/**
 * features/ui/StatTile.tsx -- one number, named.
 *
 * Template: example.html:150-224, four of them in a `grid-cols-1 md:grid-cols-2
 * lg:grid-cols-4`. Each was ~18 lines of repeated markup differing only in the
 * label, the icon and the icon's colour -- the single clearest thing in the
 * template to turn into a component.
 *
 * `value` accepts a node, not just a string, so a tile can hold a unit at a
 * smaller size without this file learning about units.
 */

import React from 'react';
import Skeleton from './Skeleton';

export type StatTone = 'success' | 'info' | 'warning' | 'error' | 'accent';

/** example.html:156 green, :177 cyan, :193 amber, :213 rose. */
const TONE: Record<StatTone, string> = {
  success: 'var(--color-success)',
  info: 'var(--color-info)',
  warning: 'var(--color-warning)',
  error: 'var(--color-error)',
  accent: 'var(--admin-accent-soft)',
};

export interface StatTileProps {
  label: string;
  value: React.ReactNode;
  icon?: React.ReactNode;
  tone?: StatTone;
  /** A secondary line: "of 348", "last 7 days". */
  caption?: React.ReactNode;
  loading?: boolean;
}

export const StatTile: React.FC<StatTileProps> = ({
  label,
  value,
  icon,
  tone = 'accent',
  caption,
  loading = false,
}) => (
  <div className="admin-card admin-card--hoverable p-5">
    <div className="flex items-start justify-between gap-3">
      <p className="text-sm font-medium text-slate-500 dark:text-slate-400">{label}</p>
      {icon && (
        <span aria-hidden="true" className="text-lg" style={{ color: TONE[tone] }}>
          {icon}
        </span>
      )}
    </div>
    {loading ? (
      <span className="mt-3 block">
        <Skeleton height="1.875rem" width="60%" />
      </span>
    ) : (
      <p className="mt-2 text-3xl font-bold text-slate-800 dark:text-slate-100">{value}</p>
    )}
    {caption && <p className="mt-1 text-xs text-slate-500 dark:text-slate-400">{caption}</p>}
  </div>
);

export default StatTile;
