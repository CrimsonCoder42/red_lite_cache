/**
 * features/ui/SecretDialog.tsx -- the show-once dialog for an API key or a temporary password.
 *
 * The server returns the secret in exactly one response (POST /admin/projects, .../keys,
 * .../rotate: internal/api/api.go createProject, tenancy.go issueKey / rotateKey; and the user
 * routes). It lives only in this dialog's closure: never in state that outlives it, never in
 * storage, never logged. Closing the dialog drops it; it cannot be shown again.
 */

import React, { useState } from 'react';
import Button from './Button';
import { FiCopy } from './icons';
import type { UseModal } from './Modal';

export interface SecretOptions {
  title: string;
  /** What the secret is ("API key", "Temporary password"). */
  label: string;
  secret: string;
  /** A ready line for a consumer's .env, when there is one. */
  envLine?: string;
  note?: React.ReactNode;
}

const CopyRow: React.FC<{ caption: string; text: string; testId: string }> = ({ caption, text, testId }) => {
  const [copied, setCopied] = useState(false);
  const copy = () => {
    void navigator.clipboard?.writeText(text).then(
      () => setCopied(true),
      () => setCopied(false),
    );
  };
  return (
    <div className="space-y-1">
      <p className="text-sm font-medium text-slate-700 dark:text-slate-300">{caption}</p>
      <div className="flex items-center gap-2">
        <code
          data-testid={testId}
          className="block min-w-0 flex-1 select-all break-all rounded-[var(--admin-radius-control)] bg-white/50 px-3 py-2 font-mono text-xs text-slate-800 dark:bg-slate-900/50 dark:text-slate-100"
        >
          {text}
        </code>
        <Button size="sm" icon={<FiCopy className="h-4 w-4" />} onClick={copy}>
          {copied ? 'Copied' : 'Copy'}
        </Button>
      </div>
    </div>
  );
};

export function showSecret(modal: UseModal, o: SecretOptions): void {
  modal.open({
    title: o.title,
    size: 'lg',
    body: () => (
      <div className="space-y-4" data-testid="secret-dialog">
        <p className="text-sm text-slate-600 dark:text-slate-300">
          This {o.label.toLowerCase()} is shown once. Copy it now: it cannot be shown again.
        </p>
        <CopyRow caption={o.label} text={o.secret} testId="secret-value" />
        {o.envLine ? <CopyRow caption=".env line" text={o.envLine} testId="secret-env-line" /> : null}
        {o.note ? <div className="text-sm text-slate-500 dark:text-slate-400">{o.note}</div> : null}
      </div>
    ),
    footer: (close) => (
      <Button variant="primary" onClick={close}>
        I have copied it
      </Button>
    ),
  });
}
