/**
 * pages/PasswordPage.tsx -- POST /admin/auth/password (internal/api/admin_auth.go changePassword).
 *
 * Forced (the user signed in with a temporary password: must_change_password, every other route
 * answers 403 until this succeeds) and voluntary (from the settings menu). The server checks the
 * current password and the 15..72-byte rule (internal/adminauth CheckPassword); this page only
 * repeats the length rule so the user sees it before sending.
 */

import React, { useState } from 'react';
import { useNavigate } from 'react-router-dom';
import { useAuth } from '@/auth/AuthContext';
import { api, ApiError } from '@/api/client';
import { Button, GlassCard, Input } from '@/features/ui';
import { useToast } from '@/features/ui/Toast';

const MIN = 15;
const MAX = 72;

export const PasswordPage: React.FC = () => {
  const { me, refresh } = useAuth();
  const toast = useToast();
  const navigate = useNavigate();
  const [current, setCurrent] = useState('');
  const [next, setNext] = useState('');
  const [again, setAgain] = useState('');
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const forced = !!me?.must_change_password;
  const bytes = new TextEncoder().encode(next).length;

  const onSubmit = async (e: React.FormEvent) => {
    e.preventDefault();
    setError(null);
    if (bytes < MIN || bytes > MAX) return setError(`The new password must be ${MIN} to ${MAX} bytes.`);
    if (next !== again) return setError('The two new passwords differ.');
    setBusy(true);
    try {
      await api('POST', '/admin/auth/password', { current_password: current, new_password: next });
      await refresh();
      toast.show('Password changed. Your other sessions were signed out.', 'success');
      navigate('/', { replace: true });
    } catch (err) {
      setError(err instanceof ApiError ? err.message : 'The admin port could not be reached.');
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="mx-auto max-w-lg">
      <GlassCard
        title={forced ? 'Choose your password' : 'Change password'}
        subtitle={forced ? 'You signed in with a temporary password. Set your own to continue.' : undefined}
      >
        <form onSubmit={onSubmit} className="space-y-4">
          <Input
            label={forced ? 'Temporary password' : 'Current password'}
            type="password"
            autoComplete="current-password"
            required
            value={current}
            onChange={(e) => setCurrent(e.target.value)}
          />
          <Input
            label="New password"
            type="password"
            autoComplete="new-password"
            required
            hint={`${MIN} to ${MAX} bytes`}
            value={next}
            onChange={(e) => setNext(e.target.value)}
          />
          <Input
            label="New password again"
            type="password"
            autoComplete="new-password"
            required
            value={again}
            onChange={(e) => setAgain(e.target.value)}
          />
          {error && (
            <p role="alert" className="text-sm text-[var(--color-error)]">
              {error}
            </p>
          )}
          <Button type="submit" variant="primary" disabled={busy}>
            {busy ? 'Saving...' : 'Save password'}
          </Button>
        </form>
      </GlassCard>
    </div>
  );
};

export default PasswordPage;
