/**
 * main.tsx -- the portal entry: React 18 root, the stylesheet, the theme listener.
 * Auth, toasts and the router are in App.tsx; the modal provider is mounted by AppShell.
 */

import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import App from './App';
import './index.css';

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <App />
  </StrictMode>,
);
