/**
 * features/ui/SearchInput.tsx -- the list toolbar's search box.
 *
 * Skin from the template's form field (example.html:512): bg-white/50 over
 * dark:bg-slate-900/50, a 1px border, indigo focus ring. The template had no
 * search input of its own -- its tables were never filtered -- so the chrome is
 * the template's and the affordance is ours.
 *
 * Replaces the search half of shared/components/list-page/ListToolbar.tsx.
 */

import React, { useId } from 'react';
import { FiSearch, FiX } from '@/features/ui/icons';

export interface SearchInputProps {
  value: string;
  onChange: (value: string) => void;
  placeholder?: string;
  /** Screen-reader label. The magnifier icon is decorative. */
  label?: string;
}

export const SearchInput: React.FC<SearchInputProps> = ({
  value,
  onChange,
  placeholder = 'Search…',
  label = 'Search this list',
}) => {
  const id = useId();
  return (
    <div className="relative w-full max-w-sm">
      <label htmlFor={id} className="sr-only">
        {label}
      </label>
      <FiSearch
        aria-hidden="true"
        className="pointer-events-none absolute left-3 top-1/2 h-4 w-4 -translate-y-1/2 text-slate-400"
      />
      <input
        id={id}
        type="search"
        value={value}
        placeholder={placeholder}
        onChange={(e) => onChange(e.target.value)}
        data-testid="list-search"
        className="w-full rounded-[var(--admin-radius-control)] border border-slate-300 bg-white/50 py-2 pl-9 pr-9 text-sm text-slate-800 placeholder:text-slate-400 focus:border-[var(--admin-accent)] focus:outline-none focus:ring-2 focus:ring-[var(--admin-accent)] dark:border-slate-600 dark:bg-slate-900/50 dark:text-slate-100"
      />
      {value && (
        <button
          type="button"
          onClick={() => onChange('')}
          aria-label="Clear search"
          className="absolute right-2 top-1/2 -translate-y-1/2 rounded p-1 text-slate-400 hover:text-slate-700 dark:hover:text-slate-200"
        >
          <FiX className="h-4 w-4" />
        </button>
      )}
    </div>
  );
};

export default SearchInput;
