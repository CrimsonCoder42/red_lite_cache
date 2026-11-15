/**
 * auth/AuthContext.tsx -- who is signed in (React context; no Redux, no stored token).
 *
 * The session is the HttpOnly cookie the server sets; this context only remembers the answer of
 * GET /admin/auth/me (internal/api/admin_auth.go me) in memory. It asks once on load; sign-in
 * (POST /admin/auth/login) and sign-out (POST /admin/auth/logout) update it; any 401 from any call
 * clears it, which sends the user to /login (ProtectedRoute).
 */

import React, { createContext, useCallback, useContext, useEffect, useMemo, useState } from 'react';
import { api, setUnauthorizedHandler } from '@/api/client';
import type { MeResponse, Role } from '@/api/types';

const RANK: Record<Role, number> = { viewer: 1, admin: 2, super_admin: 3 };

/** The server's role order (internal/adminauth Role): a higher role may do what a lower one may. */
export const atLeast = (role: Role | undefined, min: Role): boolean => !!role && RANK[role] >= RANK[min];

interface AuthValue {
  me: MeResponse | null;
  /** false until the first GET /admin/auth/me answered. */
  ready: boolean;
  login: (email: string, password: string) => Promise<void>;
  logout: () => Promise<void>;
  refresh: () => Promise<void>;
}

const AuthContext = createContext<AuthValue | null>(null);

export const useAuth = (): AuthValue => {
  const ctx = useContext(AuthContext);
  if (!ctx) throw new Error('useAuth must be used within an AuthProvider');
  return ctx;
};

export const AuthProvider: React.FC<{ children: React.ReactNode }> = ({ children }) => {
  const [me, setMe] = useState<MeResponse | null>(null);
  const [ready, setReady] = useState(false);

  const refresh = useCallback(async () => {
    try {
      setMe(await api<MeResponse>('GET', '/admin/auth/me'));
    } catch {
      setMe(null);
    } finally {
      setReady(true);
    }
  }, []);

  useEffect(() => {
    setUnauthorizedHandler(() => setMe(null));
    void refresh();
    return () => setUnauthorizedHandler(null);
  }, [refresh]);

  const login = useCallback(async (email: string, password: string) => {
    setMe(await api<MeResponse>('POST', '/admin/auth/login', { email, password }));
  }, []);

  const logout = useCallback(async () => {
    try {
      await api('POST', '/admin/auth/logout');
    } finally {
      setMe(null);
    }
  }, []);

  const value = useMemo(() => ({ me, ready, login, logout, refresh }), [me, ready, login, logout, refresh]);
  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>;
};
