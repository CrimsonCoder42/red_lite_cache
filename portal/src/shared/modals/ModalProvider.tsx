import React, { createContext, useContext, useState, useCallback } from 'react';
import { createPortal } from 'react-dom';
import BaseModal from './BaseModal';
import type { NewModalConfig, NewModalEntry, NewModalContextValue } from './types';

// ═══════════════════════════════════════════════════════════════════════════
// CONTEXT
// ═══════════════════════════════════════════════════════════════════════════

let newModalCounter = 0;

const NewModalContext = createContext<NewModalContextValue | null>(null);

export const useNewModalContext = (): NewModalContextValue => {
  const ctx = useContext(NewModalContext);
  if (!ctx) {
    throw new Error('useNewModalContext must be used within a NewModalProvider');
  }
  return ctx;
};

// ═══════════════════════════════════════════════════════════════════════════
// PROVIDER
// ═══════════════════════════════════════════════════════════════════════════

export const NewModalProvider: React.FC<{ children: React.ReactNode }> = ({
  children,
}) => {
  const [modals, setModals] = useState<NewModalEntry[]>([]);

  const openModal = useCallback((config: NewModalConfig): string => {
    const id = config.id ?? `new-modal-${++newModalCounter}`;
    setModals((prev) => [...prev, { id, config }]);
    return id;
  }, []);

  const closeModal = useCallback((id?: string) => {
    setModals((prev) => {
      if (prev.length === 0) return prev;

      if (id) {
        const entry = prev.find((m) => m.id === id);
        entry?.config.onClose?.();
        return prev.filter((m) => m.id !== id);
      }

      // Pop topmost
      const topmost = prev[prev.length - 1];
      topmost.config.onClose?.();
      return prev.slice(0, -1);
    });
  }, []);

  const closeAllModals = useCallback(() => {
    setModals((prev) => {
      prev.forEach((m) => m.config.onClose?.());
      return [];
    });
  }, []);

  const value: NewModalContextValue = {
    modals,
    openModal,
    closeModal,
    closeAllModals,
  };

  return (
    <NewModalContext.Provider value={value}>
      {children}
      {createPortal(
        <>
          {modals.map((entry, index) => (
            <React.Fragment key={entry.id}>
              {typeof entry.config.content === 'function'
                ? entry.config.content(() => closeModal(entry.id))
                : <BaseModal onClose={() => closeModal(entry.id)} size={entry.config.size} stackIndex={index} totalInStack={modals.length}>{entry.config.content}</BaseModal>
              }
            </React.Fragment>
          ))}
        </>,
        document.body
      )}
    </NewModalContext.Provider>
  );
};
