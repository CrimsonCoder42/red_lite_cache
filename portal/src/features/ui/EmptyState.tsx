/**
 * features/ui/EmptyState.tsx -- nothing to show, said properly.
 *
 * Another gap the template left (audit §4): its tables were never empty, so it
 * never had to say so. Root CLAUDE.md § Code standards requires the section to
 * render anyway -- title, controls, empty state -- which is why this is a
 * component and not an early `return null`.
 *
 * `error` is the same shape deliberately. An empty result and a failed fetch are
 * the same shaped hole in the page, and giving them two components is how one of
 * them ends up styled differently by accident. The tone is what differs.
 */

import React from 'react';

export interface EmptyStateProps {
  title: string;
  /** One line on what to do about it. */
  description?: React.ReactNode;
  icon?: React.ReactNode;
  action?: React.ReactNode;
  /** 'error' paints the icon and title with --color-error. */
  variant?: 'empty' | 'error';
}

export const EmptyState: React.FC<EmptyStateProps> = ({
  title,
  description,
  icon,
  action,
  variant = 'empty',
}) => {
  const isError = variant === 'error';
  return (
    <div
      data-testid={isError ? 'error-state' : 'empty-state'}
      role={isError ? 'alert' : 'status'}
      className="flex flex-col items-center justify-center gap-3 px-6 py-14 text-center"
    >
      {icon && (
        <span
          aria-hidden="true"
          className="text-3xl"
          style={{ color: isError ? 'var(--color-error)' : 'var(--admin-fg-muted)' }}
        >
          {icon}
        </span>
      )}
      <p
        className="text-base font-medium"
        style={{ color: isError ? 'var(--color-error)' : undefined }}
      >
        {title}
      </p>
      {description && (
        <p className="max-w-md text-sm text-slate-500 dark:text-slate-400">{description}</p>
      )}
      {action}
    </div>
  );
};

export default EmptyState;
