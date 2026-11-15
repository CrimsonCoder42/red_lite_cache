/**
 * features/ui/Skeleton.tsx -- the loading placeholder.
 *
 * The template had no loading state at all: every table was filled synchronously
 * from a local array (example.js:407-445), so there was never a moment with no
 * data. Real pages fetch, and the audit listed this as one of the gaps the
 * template does not cover.
 *
 * The pulse is CSS (`admin-skeleton`, index.css:357) and is switched off under
 * prefers-reduced-motion by index.css:370-379, so it does not make screenshots
 * unstable.
 */

import React from 'react';

export interface SkeletonProps {
  /** Any CSS length. Defaults to full width. */
  width?: string;
  height?: string;
  rounded?: boolean;
  className?: string;
}

export const Skeleton: React.FC<SkeletonProps> = ({
  width = '100%',
  height = '1rem',
  rounded = false,
  className = '',
}) => (
  <span
    aria-hidden="true"
    className={['admin-skeleton block', className].filter(Boolean).join(' ')}
    style={{ width, height, borderRadius: rounded ? 'var(--admin-radius-pill)' : undefined }}
  />
);

/**
 * Rows sized to a table body, so a loading table keeps the height it will have
 * when the data lands and the page does not jump.
 */
export const SkeletonRows: React.FC<{ rows?: number; columns?: number }> = ({
  rows = 5,
  columns = 4,
}) => (
  <div role="status" aria-label="Loading" className="space-y-3 p-6">
    {Array.from({ length: rows }, (_, r) => (
      <div key={r} className="flex gap-4">
        {Array.from({ length: columns }, (_, c) => (
          <Skeleton key={c} height="0.875rem" width={c === 0 ? '28%' : '20%'} />
        ))}
      </div>
    ))}
  </div>
);

export default Skeleton;
