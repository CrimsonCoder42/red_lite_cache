/**
 * features/ui/GlassCard.tsx -- the surface everything sits on.
 *
 * Template: example.css:190-227 (the skin, now .admin-card in index.css:328)
 * and example.html:227-247 (the header / body split, with the header's action
 * button on the right).
 *
 * The header ALWAYS renders when a title is given, including while loading and
 * on an empty result. reference/.claude/CLAUDE.md § Code standards: "UI
 * sections always render: title, controls, empty state. Never return null on
 * empty data." A card that vanishes when its fetch returns nothing reads as a
 * broken page rather than an empty one.
 */

import React from 'react';

export interface GlassCardProps {
  /** Rendered as an h2 -- the Topbar owns the page's only h1. */
  title?: React.ReactNode;
  subtitle?: React.ReactNode;
  /** Top-right controls: a filter, a "view all", a count. */
  actions?: React.ReactNode;
  /** Drops the body padding, for a table that should meet the card's edges. */
  flush?: boolean;
  hoverable?: boolean;
  className?: string;
  children?: React.ReactNode;
}

export const GlassCard: React.FC<GlassCardProps> = ({
  title,
  subtitle,
  actions,
  flush = false,
  hoverable = false,
  className = '',
  children,
}) => {
  const hasHeader = Boolean(title || subtitle || actions);

  return (
    <section
      className={['admin-card', hoverable ? 'admin-card--hoverable' : '', className]
        .filter(Boolean)
        .join(' ')}
    >
      {hasHeader && (
        <header className="flex items-start justify-between gap-4 border-b border-white/10 px-6 py-4">
          <div className="min-w-0">
            {title && (
              <h2 className="truncate text-base font-semibold text-slate-800 dark:text-slate-100">
                {title}
              </h2>
            )}
            {subtitle && (
              <p className="mt-0.5 truncate text-sm text-slate-500 dark:text-slate-400">{subtitle}</p>
            )}
          </div>
          {actions ? <div className="flex shrink-0 items-center gap-2">{actions}</div> : null}
        </header>
      )}
      <div className={flush ? '' : 'p-6'}>{children}</div>
    </section>
  );
};

export default GlassCard;
