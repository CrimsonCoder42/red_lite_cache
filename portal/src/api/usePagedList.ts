/**
 * api/usePagedList.ts -- one keyset-paged list (protocol.Page: items + next_cursor).
 *
 * Forward is the server's next_cursor; back is the cursor this hook remembered for the previous
 * page. `query` is the list's path with its filters; changing it starts over at page 1.
 */

import { useCallback, useEffect, useState } from 'react';
import { api } from './client';
import type { Page } from './types';
import type { PaginationState } from '@/features/ui/DataTable';

export interface PagedList<T> {
  rows: T[];
  loading: boolean;
  error: string | null;
  reload: () => void;
  pagination: PaginationState;
}

export function usePagedList<T>(query: string, limit = 50): PagedList<T> {
  const [cursors, setCursors] = useState<string[]>(['']); // cursors[i] = the cursor of page i+1
  const [page, setPage] = useState<Page<T>>({ items: [] });
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [tick, setTick] = useState(0);

  useEffect(() => setCursors(['']), [query]);

  const current = cursors[cursors.length - 1];
  useEffect(() => {
    let live = true;
    setLoading(true);
    const sep = query.includes('?') ? '&' : '?';
    const url = `${query}${sep}limit=${limit}${current ? `&cursor=${encodeURIComponent(current)}` : ''}`;
    api<Page<T>>('GET', url)
      .then((p) => live && (setPage(p), setError(null)))
      .catch((e: Error) => live && setError(e.message))
      .finally(() => live && setLoading(false));
    return () => {
      live = false;
    };
  }, [query, current, limit, tick]);

  const reload = useCallback(() => setTick((t) => t + 1), []);

  return {
    rows: page.items ?? [],
    loading,
    error,
    reload,
    pagination: {
      page: cursors.length,
      hasPrev: cursors.length > 1,
      hasNext: !!page.next_cursor,
      onPrev: () => setCursors((c) => (c.length > 1 ? c.slice(0, -1) : c)),
      onNext: () => page.next_cursor && setCursors((c) => [...c, page.next_cursor!]),
    },
  };
}
