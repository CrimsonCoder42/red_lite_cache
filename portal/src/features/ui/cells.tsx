/**
 * features/ui/cells.tsx -- the cell renderers every admin table shares.
 *
 * The legacy configs each declared their own. userAdminConfig.tsx had UserCell,
 * EmailCell and TenantBadge; roleAdminConfig.tsx had its own CategoryBadge and
 * RoleStatusBadge; blog and campaign had theirs -- four files, each carrying its
 * own copy of "a pill", "a muted date", "a truncated string", every one written
 * as `isDark ? 'bg-green-900/30 text-green-400' : 'bg-green-100 text-green-700'`.
 * 392 hardcoded colour decisions across nine config files.
 *
 * These are the replacements. They take data, not a theme flag: the dark values
 * live in the token set (index.css:196-224) and in Tailwind's `dark:` variant,
 * so a cell never branches on `isDark` and can never disagree with the rest of
 * the page about what dark looks like.
 */

import React from 'react';
import Badge, { type BadgeTone } from './Badge';

/** Muted secondary text -- dates, counts, anything that is not the row's subject. */
export const Muted: React.FC<{ children: React.ReactNode; className?: string }> = ({
  children,
  className = '',
}) => (
  <span className={`text-sm text-slate-500 dark:text-slate-400 ${className}`}>{children}</span>
);

/** The row's subject -- the one value a person scans the column for. */
export const Primary: React.FC<{ children: React.ReactNode }> = ({ children }) => (
  <span className="font-medium text-slate-800 dark:text-slate-100">{children}</span>
);

/** An id, token or slug. Monospace because it is compared character by character. */
export const Mono: React.FC<{ children: React.ReactNode }> = ({ children }) => (
  <span className="font-mono text-xs text-slate-600 dark:text-slate-300">{children}</span>
);

export const Truncated: React.FC<{ text?: string | null; max?: string }> = ({
  text,
  max = '14rem',
}) => {
  if (!text) return <span className="text-slate-400 dark:text-slate-500">--</span>;
  return (
    <span className="inline-block truncate align-bottom text-sm text-slate-600 dark:text-slate-300" style={{ maxWidth: max }} title={text}>
      {text}
    </span>
  );
};

export const DateCell: React.FC<{ iso?: string | null }> = ({ iso }) => {
  if (!iso) return <span className="text-slate-400 dark:text-slate-500">--</span>;
  const d = new Date(iso);
  return <Muted>{Number.isNaN(d.getTime()) ? iso : d.toLocaleDateString()}</Muted>;
};

/**
 * Avatar. The gradient is the brand pair already used across the package
 * (--color-logo-blue-500 → --color-logo-purple-500, index.css:40,45), read from
 * tokens rather than the literal `linear-gradient(135deg, #0f70f0 0%, #6240e8
 * 100%)` that userAdminConfig.tsx:44 and adminLayoutConfig.tsx:107 each carried
 * their own copy of.
 */
export const Avatar: React.FC<{ name: string; src?: string | null; size?: number }> = ({
  name,
  src,
  size = 32,
}) => {
  const initial = (name || '?').charAt(0).toUpperCase();
  if (src) {
    return (
      <img
        src={src}
        alt=""
        width={size}
        height={size}
        className="shrink-0 rounded-full object-cover"
        style={{ width: size, height: size }}
      />
    );
  }
  return (
    <span
      aria-hidden="true"
      className="flex shrink-0 items-center justify-center rounded-full text-sm font-medium text-white"
      style={{
        width: size,
        height: size,
        background:
          'linear-gradient(135deg, var(--color-logo-blue-500) 0%, var(--color-logo-purple-500) 100%)',
      }}
    >
      {initial}
    </span>
  );
};

/** Avatar + name + a secondary line. The "who is this row about" cell. */
export const IdentityCell: React.FC<{
  name: string;
  secondary?: string | null;
  imageUrl?: string | null;
}> = ({ name, secondary, imageUrl }) => (
  <div className="flex min-w-0 items-center gap-3">
    <Avatar name={name} src={imageUrl} />
    <div className="min-w-0">
      <p className="truncate font-medium text-slate-800 dark:text-slate-100">{name}</p>
      {secondary && <p className="truncate text-xs text-slate-500 dark:text-slate-400">{secondary}</p>}
    </div>
  </div>
);

/** A yes/no state -- verified, active, published. One tone pair, everywhere. */
export const BoolBadge: React.FC<{ on: boolean; onLabel: string; offLabel: string }> = ({
  on,
  onLabel,
  offLabel,
}) => <Badge tone={on ? 'success' : 'warning'}>{on ? onLabel : offLabel}</Badge>;

/** A category, plan or status word with a caller-chosen tone. */
export const TagBadge: React.FC<{ label?: string | null; tone?: BadgeTone }> = ({
  label,
  tone = 'neutral',
}) => {
  if (!label) return <span className="text-slate-400 dark:text-slate-500">--</span>;
  return (
    <Badge tone={tone} dot={false}>
      <span className="rounded-[var(--admin-radius-pill)] bg-slate-200/70 px-2 py-0.5 text-xs dark:bg-slate-700/50">
        {label}
      </span>
    </Badge>
  );
};
