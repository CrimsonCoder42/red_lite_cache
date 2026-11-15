/**
 * features/ui/Modal.tsx -- the template's dialog chrome, on the existing modal system.
 *
 * ONE MODAL SYSTEM. The package already has a complete one:
 * shared/modals/ModalProvider.tsx (a single portal, a stack, close-topmost) and
 * shared/modals/BaseModal.tsx (scroll lock ref-counted across nested modals,
 * Escape on the topmost only, a focus trap, and a bottom-sheet form under 768px).
 * None of that is rebuilt here. This file supplies the template's HEADER AND SKIN
 * (example.html:477-489: title on the left, a close button on the right, a body
 * capped at 70vh that scrolls) and opens through the provider that already exists.
 *
 * Building a second portal was the obvious wrong turn -- the template has its own
 * open/close pair at example.js:158-170 and a second one at :281-297 for its
 * contact dialog, which is exactly the duplication this rebuild exists to stop.
 */

import React from 'react';
import { FiX } from '@/features/ui/icons';
import BaseModal from '@/shared/modals/BaseModal';
import { useNewModalContext } from '@/shared/modals';
import type { ModalSize } from '@/shared/modals/types';

export interface ModalOptions {
  title: React.ReactNode;
  /** Receives the close handler, so a footer button can dismiss its own dialog. */
  body: (close: () => void) => React.ReactNode;
  size?: ModalSize;
  footer?: (close: () => void) => React.ReactNode;
}

/** example.html:479-487 */
const ModalHeader: React.FC<{ title: React.ReactNode; onClose: () => void }> = ({
  title,
  onClose,
}) => (
  <div className="flex items-center justify-between gap-4 border-b border-white/10 px-6 py-4">
    <h2 className="text-lg font-semibold text-slate-800 dark:text-slate-200">{title}</h2>
    <button
      type="button"
      onClick={onClose}
      aria-label="Close dialog"
      className="rounded p-1 text-slate-500 transition-colors hover:text-slate-800 dark:text-slate-400 dark:hover:text-slate-200"
    >
      <FiX className="h-5 w-5" />
    </button>
  </div>
);

export interface UseModal {
  open: (options: ModalOptions) => string;
  close: (id?: string) => void;
}

export function useModal(): UseModal {
  const { openModal, closeModal } = useNewModalContext();

  return {
    open: (options) =>
      openModal({
        /* The render-function form of NewModalConfig (shared/modals/types.ts:44),
           so this file supplies its own BaseModal and therefore its own skin,
           while the provider keeps owning the portal and the stack. */
        content: (close) => (
          <BaseModal
            onClose={close}
            size={options.size ?? 'lg'}
            className="admin-card"
            ariaLabel={typeof options.title === 'string' ? options.title : undefined}
            header={<ModalHeader title={options.title} onClose={close} />}
            footer={
              options.footer ? (
                <div className="flex justify-end gap-2 border-t border-white/10 px-6 py-4">
                  {options.footer(close)}
                </div>
              ) : undefined
            }
          >
            {/* example.html:489 -- max-h-[70vh] overflow-y-auto. BaseModal's body
                slot already scrolls, so only the padding is ours. */}
            <div className="p-6">{options.body(close)}</div>
          </BaseModal>
        ),
      }),
    close: closeModal,
  };
}

export default useModal;
