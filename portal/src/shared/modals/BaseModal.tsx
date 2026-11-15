/**
 * shared/modals/BaseModal.tsx -- copied from the reference admin; trimmed: no motion package,
 * no mobile bottom sheet, the theme read from useAdminTheme (see the note at the return).
 */
import React, { useEffect, useRef, useCallback } from 'react';
import { useAdminTheme } from '@/features/theme/useAdminTheme';
import type { BaseModalProps, ModalSize } from './types';

// ═══════════════════════════════════════════════════════════════════════════
// SIZE MAP
// ═══════════════════════════════════════════════════════════════════════════

const SIZE_CLASSES: Record<ModalSize, string> = {
  sm: 'max-w-sm',
  md: 'max-w-md',
  lg: 'max-w-lg',
  xl: 'max-w-xl',
  full: 'max-w-4xl',
};

// ═══════════════════════════════════════════════════════════════════════════
// SCROLL LOCK (ref-counted so nested modals don't fight)
// ═══════════════════════════════════════════════════════════════════════════

let scrollLockCount = 0;
let savedOverflow = '';

function lockScroll(): void {
  if (scrollLockCount === 0) {
    savedOverflow = document.body.style.overflow;
    document.body.style.overflow = 'hidden';
  }
  scrollLockCount++;
}

function unlockScroll(): void {
  scrollLockCount--;
  if (scrollLockCount <= 0) {
    scrollLockCount = 0;
    document.body.style.overflow = savedOverflow;
  }
}

// ═══════════════════════════════════════════════════════════════════════════
// BASE MODAL
// ═══════════════════════════════════════════════════════════════════════════

const BaseModal: React.FC<BaseModalProps> = ({
  onClose,
  size = 'md',
  closeOnBackdrop = true,
  closeOnEsc = true,
  className,
  stackIndex = 0,
  totalInStack = 1,
  header,
  chartContent,
  children,
  footer,
  ariaLabel,
}) => {
  const { isDark } = useAdminTheme();
  const panelRef = useRef<HTMLDivElement>(null);
  const isTopModal = stackIndex === (totalInStack - 1);
  const zIndex = 100 + stackIndex * 10;

  // ─── Focus panel on mount ───────────────────────────────────────────
  useEffect(() => {
    if (isTopModal && panelRef.current) {
      panelRef.current.focus();
    }
  }, [isTopModal]);

  // ─── Scroll lock ────────────────────────────────────────────────────
  useEffect(() => {
    lockScroll();
    return () => unlockScroll();
  }, []);

  // ─── ESC handler (topmost only) ────────────────────────────────────
  const handleKeyDown = useCallback(
    (e: React.KeyboardEvent) => {
      if (e.key === 'Escape' && closeOnEsc && isTopModal) {
        e.stopPropagation();
        onClose();
      }
    },
    [closeOnEsc, isTopModal, onClose]
  );

  // ─── Focus trap ─────────────────────────────────────────────────────
  const handleTab = useCallback(
    (e: React.KeyboardEvent) => {
      if (e.key !== 'Tab' || !isTopModal || !panelRef.current) return;

      const focusable = panelRef.current.querySelectorAll<HTMLElement>(
        'button, [href], input, select, textarea, [tabindex]:not([tabindex="-1"])'
      );
      if (focusable.length === 0) return;

      const first = focusable[0];
      const last = focusable[focusable.length - 1];

      if (e.shiftKey) {
        if (document.activeElement === first) {
          e.preventDefault();
          last.focus();
        }
      } else {
        if (document.activeElement === last) {
          e.preventDefault();
          first.focus();
        }
      }
    },
    [isTopModal]
  );

  const combinedKeyHandler = (e: React.KeyboardEvent) => {
    handleKeyDown(e);
    handleTab(e);
  };

  const handleBackdropClick = () => {
    if (closeOnBackdrop) onClose();
  };

  // ─── Border classes ─────────────────────────────────────────────────
  const borderClass = isDark ? 'border-slate-700' : 'border-slate-200';

  // ─── Shared panel content ───────────────────────────────────────────
  const panelContent = (
    <>

      {/* Header slot */}
      {header && <div className="shrink-0">{header}</div>}

      {/* Chart content slot */}
      {chartContent && <div className="shrink-0">{chartContent}</div>}

      {/* Scrollable body */}
      <div className="flex-1 min-h-0 overflow-y-auto">
        {children}
      </div>

      {/* Footer slot */}
      {footer && <div className="shrink-0">{footer}</div>}
    </>
  );

  // One centered dialog at every width. The reference animates it with the `motion` package (and
  // turns it into a bottom sheet under 768px); this portal may not add that package, so the dialog
  // uses the reference stylesheet's own CSS animations instead: overlay-fade-in on the backdrop
  // and modal-scale-in on the panel (index.css), and closes without an exit animation.
  return (
    <div
      className="fixed inset-0 flex items-center justify-center p-4 modal-overlay-opening"
      style={{ zIndex }}
    >
      {/* Backdrop */}
      <div
        className={`absolute inset-0 ${
          stackIndex === 0 ? 'bg-black/50' : 'bg-black/25'
        }`}
        onClick={handleBackdropClick}
        aria-hidden="true"
      />

      {/* Centered panel */}
      <div
        ref={panelRef}
        role="dialog"
        aria-modal="true"
        aria-label={ariaLabel}
        tabIndex={-1}
        onKeyDown={combinedKeyHandler}
        className={`relative w-full ${SIZE_CLASSES[size]} flex flex-col rounded-xl shadow-lg border ${borderClass} ${isDark ? 'bg-gray-800' : 'bg-white'} max-h-[80vh] focus:outline-none modal-scale-in ${className ?? ''}`}
      >
        {panelContent}
      </div>
    </div>
  );
};

export default BaseModal;
