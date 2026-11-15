/**
 * features/ui/FilterTabs.tsx -- the "All / Published / Draft" row above a list.
 *
 * Replaces shared/components/list-page/ListFilters.tsx.
 *
 * The template's nearest thing is its sidebar's active-link treatment
 * (example.js:121-130) -- a tinted pill and a colour change -- so that is what a
 * selected tab gets, rather than the underline the legacy filters used. One
 * active-state language across the app.
 *
 * It is a real tablist: arrow keys move between tabs, and `aria-selected` says
 * which is current. The legacy version was a row of unlabelled buttons.
 */

import React from 'react';

export interface FilterTab {
  key: string;
  label: string;
  count?: number;
}

export interface FilterTabsProps {
  tabs: FilterTab[];
  activeKey: string;
  onChange: (key: string) => void;
  /** Names the group for a screen reader -- "Filter blog posts". */
  label: string;
}

export const FilterTabs: React.FC<FilterTabsProps> = ({ tabs, activeKey, onChange, label }) => {
  const onKeyDown = (e: React.KeyboardEvent, index: number): void => {
    if (e.key !== 'ArrowRight' && e.key !== 'ArrowLeft') return;
    e.preventDefault();
    const next = e.key === 'ArrowRight' ? (index + 1) % tabs.length : (index - 1 + tabs.length) % tabs.length;
    onChange(tabs[next].key);
  };

  return (
    <div role="tablist" aria-label={label} className="flex flex-wrap items-center gap-2">
      {tabs.map((tab, i) => {
        const active = tab.key === activeKey;
        return (
          <button
            key={tab.key}
            role="tab"
            type="button"
            aria-selected={active}
            onClick={() => onChange(tab.key)}
            onKeyDown={(e) => onKeyDown(e, i)}
            className={[
              'flex items-center gap-2 rounded-[var(--admin-radius-control)] px-3 py-1.5 text-sm transition-colors',
              active
                ? 'bg-slate-200/80 font-medium text-slate-900 dark:bg-slate-700/50 dark:text-slate-100'
                : 'text-slate-600 hover:bg-slate-200/50 hover:text-slate-900 dark:text-slate-400 dark:hover:bg-slate-700/30 dark:hover:text-slate-200',
            ].join(' ')}
          >
            {tab.label}
            {tab.count !== undefined && (
              <span
                className={[
                  'rounded-[var(--admin-radius-pill)] px-1.5 py-0.5 text-xs',
                  active
                    ? 'bg-[var(--admin-accent)] text-[var(--admin-on-accent)]'
                    : 'bg-slate-200/70 text-slate-600 dark:bg-slate-700/50 dark:text-slate-300',
                ].join(' ')}
              >
                {tab.count}
              </span>
            )}
          </button>
        );
      })}
    </div>
  );
};

export default FilterTabs;
