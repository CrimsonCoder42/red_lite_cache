/**
 * auth/ProtectedRoute.tsx -- trimmed from the reference admin's auth/components/ProtectedRoute.tsx:
 * the same loading spinner and the same three gates, read from the auth context instead of Redux.
 *
 *   not signed in              -> /login (remembering where the user was going)
 *   must change the password   -> /password (the server refuses everything else: 403)
 *   below the page's role      -> /  (the nav hides such pages; this covers a typed URL)
 */

import React from 'react';
import { Navigate, useLocation } from 'react-router-dom';
import type { Role } from '@/api/types';
import { atLeast, useAuth } from './AuthContext';

interface ProtectedRouteProps {
  children: React.ReactNode;
  minRole?: Role;
}

export const ProtectedRoute: React.FC<ProtectedRouteProps> = ({ children, minRole = 'viewer' }) => {
  const location = useLocation();
  const { me, ready } = useAuth();

  if (!ready) {
    return (
      <div className="flex flex-col items-center justify-center min-h-screen bg-gray-50 dark:bg-slate-900">
        <div className="w-12 h-12 border-4 border-gray-200 border-t-blue-600 rounded-full animate-spin dark:border-slate-700 dark:border-t-blue-500" />
        <p className="mt-4 text-sm text-gray-500 dark:text-slate-400">Loading...</p>
      </div>
    );
  }

  if (!me) {
    return <Navigate to="/login" state={{ from: location }} replace />;
  }

  if (me.must_change_password && location.pathname !== '/password') {
    return <Navigate to="/password" replace />;
  }

  if (!atLeast(me.role, minRole)) {
    return <Navigate to="/" replace />;
  }

  return <>{children}</>;
};

export default ProtectedRoute;
