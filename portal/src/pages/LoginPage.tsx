/**
 * pages/LoginPage.tsx -- POST /admin/auth/login (internal/api/admin_auth.go login).
 *
 * Trimmed from the reference admin's auth/pages/LoginPage.tsx: its right-hand form pane only (the
 * admin-shell ground, the admin-card, the same fields and the gradient button). Left behind: the
 * hero image pane, Google sign-in, remember me, forgot password, verification and sign-up links
 * (the admin port has none of them). Every failure reads the server's one reply ("sign-in failed").
 */

import React, { useState } from 'react';
import { Navigate, useLocation, useNavigate } from 'react-router-dom';
import { useAuth } from '@/auth/AuthContext';
import { ApiError } from '@/api/client';

const FIELD =
  'block w-full px-4 py-3 border border-gray-300 rounded-lg focus:ring-2 focus:ring-blue-500 focus:border-transparent transition-colors bg-white text-gray-900';

export const LoginPage: React.FC = () => {
  const { me, login } = useAuth();
  const navigate = useNavigate();
  const location = useLocation();
  const [email, setEmail] = useState('');
  const [password, setPassword] = useState('');
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  if (me) return <Navigate to="/" replace />;

  const from = (location.state as { from?: { pathname?: string } } | null)?.from?.pathname ?? '/';

  const onSubmit = async (e: React.FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError(null);
    try {
      await login(email, password);
      navigate(from === '/login' ? '/' : from, { replace: true });
    } catch (err) {
      setError(err instanceof ApiError ? err.message : 'The admin port could not be reached.');
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="min-h-screen flex">
      <div className="w-full flex items-center justify-center p-8 relative admin-shell">
        <div className="relative z-10 w-full max-w-md">
          <div className="text-center mb-8">
            <h1 className="text-3xl font-bold">
              <span style={{ color: 'var(--color-logo-blue-500)' }}>red_lite</span>
              <span style={{ color: 'var(--color-logo-purple-500)' }}>_cache</span>
            </h1>
          </div>

          <div className="admin-card shadow-[var(--admin-shadow)] p-8">
            <div className="text-center mb-8">
              <h2 className="text-2xl font-bold text-slate-900 dark:text-slate-100">Sign in to the admin portal</h2>
            </div>

            <form onSubmit={onSubmit} className="space-y-5">
              <div>
                <label htmlFor="email" className="mb-1 block text-sm font-medium text-slate-700 dark:text-slate-300">
                  Email address
                </label>
                <input
                  id="email"
                  name="email"
                  type="email"
                  autoComplete="username"
                  required
                  value={email}
                  onChange={(e) => setEmail(e.target.value)}
                  className={FIELD}
                  placeholder="Enter your email"
                />
              </div>

              <div>
                <label htmlFor="password" className="mb-1 block text-sm font-medium text-slate-700 dark:text-slate-300">
                  Password
                </label>
                <input
                  id="password"
                  name="password"
                  type="password"
                  autoComplete="current-password"
                  required
                  value={password}
                  onChange={(e) => setPassword(e.target.value)}
                  className={FIELD}
                  placeholder="Enter your password"
                />
              </div>

              {error && (
                <div className="p-3 bg-red-50 border border-red-200 rounded-lg" role="alert">
                  <p className="text-sm text-red-600">{error}</p>
                </div>
              )}

              <button
                type="submit"
                disabled={busy}
                className="w-full py-3 px-4 text-white font-semibold rounded-lg transition-all duration-200 disabled:opacity-50 disabled:cursor-not-allowed hover:opacity-90 hover:shadow-lg"
                style={{ background: 'linear-gradient(135deg, #0f70f0 0%, #6240e8 100%)' }}
              >
                {busy ? (
                  <div className="flex items-center justify-center gap-2">
                    <div className="w-5 h-5 border-2 border-white border-t-transparent rounded-full animate-spin" />
                    <span>Signing in...</span>
                  </div>
                ) : (
                  'Sign in'
                )}
              </button>
            </form>
          </div>
        </div>
      </div>
    </div>
  );
};

export default LoginPage;
