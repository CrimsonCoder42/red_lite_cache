/**
 * features/ui/TablePager.tsx -- the footer under a DataTable, trimmed from the reference admin's
 * offset/limit pager to the admin port's keyset cursors: every list answers {items, next_cursor}
 * (protocol.Page), so the pager knows only "is there a next page" and "have we gone forward".
 * Same markup and classes as the reference; the range text becomes the page number.
 */

import React from 'react';

export interface TablePagerProps {
  /** 1-based page currently shown. */
  page: number;
  hasPrev: boolean;
  hasNext: boolean;
  onPrev: () => void;
  onNext: () => void;
  /** Rows actually rendered. */
  shown: number;
}

const BTN =
  'rounded px-2 py-1 transition-colors enabled:hover:bg-slate-200/60 ' +
  'disabled:cursor-not-allowed disabled:opacity-40 dark:enabled:hover:bg-slate-700/40';

export const TablePager: React.FC<TablePagerProps> = ({ page, hasPrev, hasNext, onPrev, onNext, shown }) => (
  <nav
    aria-label="Pagination"
    className="flex items-center justify-between gap-4 border-t border-white/10 px-6 py-3 text-xs text-slate-500 dark:text-slate-400"
  >
    <span data-testid="pager-range">
      Page {page} -- {shown} shown
    </span>
    <span className="flex gap-2">
      <button type="button" className={BTN} disabled={!hasPrev} onClick={onPrev}>
        Previous
      </button>
      <button type="button" className={BTN} disabled={!hasNext} onClick={onNext}>
        Next
      </button>
    </span>
  </nav>
);

export default TablePager;
