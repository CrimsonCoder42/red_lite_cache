/**
 * features/ui/Badge.tsx -- a status dot and its label.
 *
 * The template's getStatusIndicator (example.js:355-365) mapped five statuses to
 * five background classes and returned an HTML string. Two things changed in the
 * port:
 *
 *  - the map is keyed by TONE, not by the word "Online". The template's five
 *    words were its own domain's; a subscriber source or a plan state needs the
 *    same five colours under different names, and a component that only knows
 *    the word "Online" gets copied instead of reused.
 *  - the colours are tokens. The template hardcoded bg-green-500 / bg-slate-500 /
 *    bg-rose-500 / bg-amber-500; these read --color-success / --color-warning /
 *    --color-error / --color-info (index.css:47-50), the values the package
 *    already had, so the admin agrees with the rest of the product.
 */

import React from 'react';

export type BadgeTone = 'success' | 'warning' | 'error' | 'info' | 'neutral';

/** The five dots, example.js:356-362 in tone form. */
const DOT: Record<BadgeTone, string> = {
  success: 'var(--color-success)',
  warning: 'var(--color-warning)',
  error: 'var(--color-error)',
  info: 'var(--color-info)',
  neutral: 'var(--admin-fg-muted)',
};

export interface BadgeProps {
  tone?: BadgeTone;
  /** Hide the dot for a plain text pill. */
  dot?: boolean;
  children: React.ReactNode;
}

export const Badge: React.FC<BadgeProps> = ({ tone = 'neutral', dot = true, children }) => (
  <span
    data-tone={tone}
    className="inline-flex items-center gap-2 whitespace-nowrap text-sm text-slate-700 dark:text-slate-300"
  >
    {dot && (
      <span
        aria-hidden="true"
        className="h-2.5 w-2.5 shrink-0 rounded-full"
        style={{ backgroundColor: DOT[tone] }}
      />
    )}
    {children}
  </span>
);

export default Badge;
