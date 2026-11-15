/**
 * features/ui/Button.tsx -- the three buttons the template actually had.
 *
 *  primary  example.html:125 -- solid indigo, the one affirmative action
 *  glass    example.css:252-291 (.login-button) -- the glass control
 *  ghost    example.html:483 -- an icon or a dismiss, no chrome until hovered
 *
 * No fourth variant is invented here. The template's "Register" button
 * (example.html:128) carried BOTH `login-button glass-ui` AND `bg-indigo-600`,
 * so its gradient was painted over by the solid fill and the glass never showed;
 * that combination is not reproduced.
 */

import React from 'react';

export type ButtonVariant = 'primary' | 'glass' | 'ghost';
export type ButtonSize = 'sm' | 'md';

const VARIANT: Record<ButtonVariant, string> = {
  primary:
    'text-[var(--admin-on-accent)] bg-[var(--admin-accent)] hover:bg-[var(--admin-accent-hover)] ' +
    'focus-visible:ring-[var(--admin-accent)]',
  glass:
    'admin-glass text-slate-700 dark:text-slate-100 hover:border-[var(--admin-accent)] ' +
    'focus-visible:ring-[var(--admin-accent)]',
  ghost:
    'text-slate-500 hover:text-slate-900 hover:bg-slate-200/60 dark:text-slate-400 ' +
    'dark:hover:text-slate-100 dark:hover:bg-slate-700/40 focus-visible:ring-slate-400',
};

const SIZE: Record<ButtonSize, string> = {
  sm: 'px-3 py-1.5 text-xs',
  md: 'px-4 py-2 text-sm',
};

export interface ButtonProps extends React.ButtonHTMLAttributes<HTMLButtonElement> {
  variant?: ButtonVariant;
  size?: ButtonSize;
  /** Leading icon. Decorative -- the label carries the meaning. */
  icon?: React.ReactNode;
}

export const Button: React.FC<ButtonProps> = ({
  variant = 'glass',
  size = 'md',
  icon,
  className = '',
  type = 'button',
  children,
  ...rest
}) => (
  <button
    type={type}
    className={[
      'inline-flex items-center justify-center gap-2 rounded-[var(--admin-radius-control)]',
      'font-medium transition-colors focus:outline-none focus-visible:ring-2 focus-visible:ring-offset-2',
      'focus-visible:ring-offset-transparent disabled:cursor-not-allowed disabled:opacity-50',
      VARIANT[variant],
      SIZE[size],
      className,
    ]
      .filter(Boolean)
      .join(' ')}
    {...rest}
  >
    {icon ? <span aria-hidden="true">{icon}</span> : null}
    {children}
  </button>
);

export default Button;
