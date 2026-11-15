/**
 * features/ui/Toast.tsx -- a small toast stack (no react-toastify).
 *
 * It renders the class names whose colours index.css copies from the reference admin
 * (.Toastify__toast--warning for 4xx, --error for 5xx and network failures, --success, --info), so a
 * toast looks as it did there. Top right, newest last, dismissed by its close button or after 6 s.
 * No animation is added.
 */

import React, { createContext, useCallback, useContext, useRef, useState } from 'react';
import { FiX } from './icons';
import { ApiError } from '@/api/client';

export type ToastKind = 'success' | 'warning' | 'error' | 'info';

interface ToastItem {
  id: number;
  kind: ToastKind;
  text: string;
}

interface ToastApi {
  show: (text: string, kind?: ToastKind) => void;
  /** An API failure: a 4xx shows the server's message as a warning, anything else as an error. */
  fail: (err: unknown) => void;
}

const ToastContext = createContext<ToastApi | null>(null);

export const useToast = (): ToastApi => {
  const ctx = useContext(ToastContext);
  if (!ctx) throw new Error('useToast must be used within a ToastProvider');
  return ctx;
};

const LIFETIME_MS = 6000;

export const ToastProvider: React.FC<{ children: React.ReactNode }> = ({ children }) => {
  const [items, setItems] = useState<ToastItem[]>([]);
  const next = useRef(0);

  const dismiss = useCallback((id: number) => setItems((prev) => prev.filter((t) => t.id !== id)), []);

  const show = useCallback(
    (text: string, kind: ToastKind = 'info') => {
      const id = ++next.current;
      setItems((prev) => [...prev, { id, kind, text }]);
      window.setTimeout(() => dismiss(id), LIFETIME_MS);
    },
    [dismiss],
  );

  const fail = useCallback(
    (err: unknown) => {
      if (err instanceof ApiError) {
        if (err.status === 401) return; // the auth context already sent the user to /login
        show(err.message, err.status < 500 ? 'warning' : 'error');
        return;
      }
      show('The admin port could not be reached.', 'error');
    },
    [show],
  );

  return (
    <ToastContext.Provider value={{ show, fail }}>
      {children}
      <div className="Toastify__toast-container fixed right-4 top-4 flex w-80 flex-col gap-2" role="status" aria-live="polite">
        {items.map((t) => (
          <div key={t.id} className={`Toastify__toast Toastify__toast--${t.kind} flex items-start gap-3`} data-testid="toast">
            <div className="Toastify__toast-body flex-1">{t.text}</div>
            <button type="button" aria-label="Dismiss" onClick={() => dismiss(t.id)} className="Toastify__close-button">
              <FiX className="h-4 w-4" />
            </button>
          </div>
        ))}
      </div>
    </ToastContext.Provider>
  );
};

export default ToastProvider;
