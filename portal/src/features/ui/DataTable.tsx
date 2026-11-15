/**
 * features/ui/DataTable.tsx -- the one table.
 *
 * Template: example.html:229-245 (plain) and :273-283 (the sticky-header
 * variant), filled by example.js:407-445 with `innerHTML +=` per row. Four
 * tables, four copies of the same thead markup, and the four legacy config
 * modules behind DynamicAdminListPage came to 2,238 lines of column definitions
 * (doc/admin/rebuild.md § never repeat). This is the replacement: columns are
 * data, the table is one component.
 *
 * SORT AND PAGINATION ARE PROPS, NOT BEHAVIOUR. Passing `sort` + `onSortChange`
 * turns headers into buttons and reports the click; this component never reorders
 * `rows`. Passing `pagination` renders the footer and reports the intent. Both
 * are server concerns here -- /admin/newsletter-subscribers pages with
 * limit/offset (reference-backend/api/handlers/admin/newsletter.go:15-17) -- and a
 * client-side sort over one page of 348 rows would be a lie about the ordering.
 */

import React from 'react';
import { SkeletonRows } from './Skeleton';
import EmptyState from './EmptyState';
import TablePager from './TablePager';

export interface Column<T> {
  key: string;
  header: React.ReactNode;
  render: (row: T) => React.ReactNode;
  /** Any CSS width. Omit to let the column share what is left. */
  width?: string;
  align?: 'left' | 'right';
  sortable?: boolean;
}

export interface SortState {
  key: string;
  direction: 'asc' | 'desc';
}

/** Keyset paging (protocol.Page next_cursor): see TablePager and usePagedList. */
export type PaginationState = Omit<import('./TablePager').TablePagerProps, 'shown'>;

export interface DataTableProps<T> {
  columns: Column<T>[];
  rows: T[];
  rowKey: (row: T) => string;
  caption: string;
  loading?: boolean;
  /** A message means the fetch failed; rows are not shown. */
  error?: string | null;
  emptyTitle?: string;
  emptyDescription?: React.ReactNode;
  /** Sticky header. example.html:276 -- for a log that scrolls inside its card. */
  sticky?: boolean;
  /** Caps the scroll box; required for `sticky` to have anything to stick to. */
  maxHeight?: string;
  sort?: SortState;
  onSortChange?: (next: SortState) => void;
  pagination?: PaginationState;
  /** Makes each row activatable. Keyboard gets it too, not just the mouse. */
  onRowClick?: (row: T) => void;
}

const CELL = 'px-6 py-4 text-sm';
const HEAD =
  'px-6 py-3 text-left text-xs font-semibold uppercase tracking-wide text-slate-500 dark:text-slate-400';

export function DataTable<T>({
  columns,
  rows,
  rowKey,
  caption,
  loading = false,
  error = null,
  emptyTitle = 'Nothing to show',
  emptyDescription,
  sticky = false,
  maxHeight,
  sort,
  onSortChange,
  pagination,
  onRowClick,
}: DataTableProps<T>): React.ReactElement {
  if (loading) return <SkeletonRows rows={6} columns={columns.length} />;
  if (error) return <EmptyState variant="error" title="Could not load this list" description={error} />;
  if (rows.length === 0) return <EmptyState title={emptyTitle} description={emptyDescription} />;

  const sortLabel = (key: string): 'ascending' | 'descending' | 'none' =>
    sort?.key === key ? (sort.direction === 'asc' ? 'ascending' : 'descending') : 'none';

  return (
    <div>
      {/* The table's own scroller. The page body must never scroll sideways. */}
      <div className="w-full overflow-x-auto" style={maxHeight ? { maxHeight, overflowY: 'auto' } : undefined}>
        <table className="w-full text-left">
          <caption className="sr-only">{caption}</caption>
          <thead
            className={[
              'bg-white/5 dark:bg-black/10',
              sticky ? 'sticky top-0 z-10 backdrop-blur-sm' : '',
            ].join(' ')}
          >
            <tr>
              {columns.map((col) => (
                <th
                  key={col.key}
                  scope="col"
                  style={col.width ? { width: col.width } : undefined}
                  aria-sort={col.sortable && sort ? sortLabel(col.key) : undefined}
                  className={[HEAD, col.align === 'right' ? 'text-right' : ''].join(' ')}
                >
                  {col.sortable && onSortChange ? (
                    <button
                      type="button"
                      className="uppercase hover:text-slate-800 dark:hover:text-slate-200"
                      onClick={() =>
                        onSortChange({
                          key: col.key,
                          direction: sort?.key === col.key && sort.direction === 'asc' ? 'desc' : 'asc',
                        })
                      }
                    >
                      {col.header}
                    </button>
                  ) : (
                    col.header
                  )}
                </th>
              ))}
            </tr>
          </thead>
          <tbody>
            {rows.map((row) => (
              <tr
                key={rowKey(row)}
                onClick={onRowClick ? () => onRowClick(row) : undefined}
                /* A clickable row is reachable by keyboard or it is not
                   reachable. tabIndex + Enter/Space, not a bare onClick. */
                tabIndex={onRowClick ? 0 : undefined}
                onKeyDown={
                  onRowClick
                    ? (e) => {
                        if (e.key === 'Enter' || e.key === ' ') {
                          e.preventDefault();
                          onRowClick(row);
                        }
                      }
                    : undefined
                }
                className={[
                  'border-b border-white/10 last:border-0',
                  onRowClick
                    ? 'cursor-pointer transition-colors hover:bg-white/10 focus:outline-none focus-visible:bg-white/10 dark:hover:bg-white/5'
                    : '',
                ].join(' ')}
              >
                {columns.map((col) => (
                  <td
                    key={col.key}
                    className={[CELL, col.align === 'right' ? 'text-right' : ''].join(' ')}
                  >
                    {col.render(row)}
                  </td>
                ))}
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      {pagination && <TablePager {...pagination} shown={rows.length} />}
    </div>
  );
}

export default DataTable;
