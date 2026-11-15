/**
 * App.tsx -- the routes. The build is served under /portal/ (vite.config.ts base), so the router's
 * basename is /portal. Every page but /login sits in the AppShell behind ProtectedRoute; a page's
 * minimum role matches the server's route table (doc/specs/s11_admin_portal.md).
 */

import React from 'react';
import { BrowserRouter, Navigate, Route, Routes } from 'react-router-dom';
import { AuthProvider } from '@/auth/AuthContext';
import ProtectedRoute from '@/auth/ProtectedRoute';
import AppShell from '@/features/layout/AppShell';
import { ToastProvider } from '@/features/ui/Toast';
import LoginPage from '@/pages/LoginPage';
import PasswordPage from '@/pages/PasswordPage';
import ProjectsPage from '@/pages/ProjectsPage';
import ProjectDetailPage from '@/pages/ProjectDetailPage';

export const App: React.FC = () => (
  <BrowserRouter basename="/portal">
    <ToastProvider>
      <AuthProvider>
        <Routes>
          <Route path="/login" element={<LoginPage />} />
          <Route
            element={
              <ProtectedRoute>
                <AppShell />
              </ProtectedRoute>
            }
          >
            <Route index element={<Navigate to="/projects" replace />} />
            <Route path="/password" element={<PasswordPage />} />
            <Route path="/projects" element={<ProjectsPage />} />
            <Route path="/projects/:id" element={<ProjectDetailPage />} />
          </Route>
          <Route path="*" element={<Navigate to="/" replace />} />
        </Routes>
      </AuthProvider>
    </ToastProvider>
  </BrowserRouter>
);

export default App;
