/**
 * features/ui/Input.tsx -- a labelled text field.
 *
 * Template: example.html:510-517. Its two inputs had a <label> and an id, but
 * nothing tied them together -- no `htmlFor` -- so clicking the label did nothing
 * and a screen reader announced an unlabelled box. Fixed here: the id is
 * generated when not supplied and `htmlFor` always points at it.
 *
 * Validation STATE only, not validation logic. An `error` string paints the
 * border and is announced; deciding what counts as invalid belongs to the form
 * that owns the field.
 */

import React, { useId } from 'react';

export interface InputProps extends Omit<React.InputHTMLAttributes<HTMLInputElement>, 'id'> {
  label: string;
  id?: string;
  /** Present = invalid. The text is shown and linked by aria-describedby. */
  error?: string;
  hint?: string;
}

export const Input: React.FC<InputProps> = ({
  label,
  id,
  error,
  hint,
  className = '',
  ...rest
}) => {
  const generated = useId();
  const inputId = id ?? `input-${generated}`;
  const describedBy = error ? `${inputId}-error` : hint ? `${inputId}-hint` : undefined;

  return (
    <div className="w-full">
      <label
        htmlFor={inputId}
        className="block text-sm font-medium text-slate-700 dark:text-slate-300"
      >
        {label}
      </label>
      <input
        id={inputId}
        aria-invalid={error ? true : undefined}
        aria-describedby={describedBy}
        className={[
          'mt-1 block w-full rounded-[var(--admin-radius-control)] px-3 py-2 text-sm',
          'bg-white/50 text-slate-800 placeholder:text-slate-400',
          'dark:bg-slate-900/50 dark:text-slate-100',
          'border focus:outline-none focus:ring-2 focus:ring-[var(--admin-accent)]',
          error
            ? 'border-[var(--color-error)]'
            : 'border-slate-300 dark:border-slate-600 focus:border-[var(--admin-accent)]',
          className,
        ]
          .filter(Boolean)
          .join(' ')}
        {...rest}
      />
      {error && (
        <p id={`${inputId}-error`} role="alert" className="mt-1 text-xs text-[var(--color-error)]">
          {error}
        </p>
      )}
      {!error && hint && (
        <p id={`${inputId}-hint`} className="mt-1 text-xs text-slate-500 dark:text-slate-400">
          {hint}
        </p>
      )}
    </div>
  );
};

export default Input;
