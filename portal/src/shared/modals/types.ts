import type { ReactNode } from 'react';

// ═══════════════════════════════════════════════════════════════════════════
// SIZE
// ═══════════════════════════════════════════════════════════════════════════

export type ModalSize = 'sm' | 'md' | 'lg' | 'xl' | 'full';

// ═══════════════════════════════════════════════════════════════════════════
// BASE MODAL PROPS (the layout component)
// ═══════════════════════════════════════════════════════════════════════════

export interface BaseModalProps {
  onClose: () => void;
  /** Default: 'md'. Ignored on mobile (always full-width bottom sheet). */
  size?: ModalSize;
  /** Default: true */
  closeOnBackdrop?: boolean;
  /** Default: true */
  closeOnEsc?: boolean;
  className?: string;
  /** Managed by NewModalProvider context. Default: 0 */
  stackIndex?: number;
  /** Managed by NewModalProvider context. Default: 1 */
  totalInStack?: number;
  /** Fixed top slot (shrink-0) */
  header?: ReactNode;
  /** Below header, above body (shrink-0, optional) */
  chartContent?: ReactNode;
  /** Scrollable body (flex-1) */
  children: ReactNode;
  /** Fixed bottom slot (shrink-0) */
  footer?: ReactNode;
  ariaLabel?: string;
}

// ═══════════════════════════════════════════════════════════════════════════
// MODAL CONTEXT TYPES (provider / consumer)
// ═══════════════════════════════════════════════════════════════════════════

export interface NewModalConfig {
  /** Auto-generated if omitted */
  id?: string;
  /**
   * Panel width when `content` is a plain node and the provider wraps it in
   * BaseModal. Ignored for the render-function form, which supplies its own.
   *
   * Three call sites already passed this and it was never in the type -- three
   * of the package's fifteen baseline typecheck errors (admin/pages/BlogPage.tsx:84
   * twice, admin/pages/UserPage.tsx:141). The prop was doing nothing at runtime
   * either, because ModalProvider never forwarded it. Both halves fixed.
   */
  size?: ModalSize;
  /** Render function (receives close handler) or direct ReactNode */
  content: ((onClose: () => void) => ReactNode) | ReactNode;
  /** Callback when modal closes */
  onClose?: () => void;
}

export interface NewModalEntry {
  id: string;
  config: NewModalConfig;
}

export interface NewConfirmOptions {
  title: string;
  message: string;
  /** Default: 'Confirm' */
  confirmLabel?: string;
  /** Default: 'Cancel' */
  cancelLabel?: string;
  /** Default: 'default' */
  variant?: 'default' | 'danger';
  /** Default: 'sm' */
  size?: ModalSize;
}

export interface NewModalContextValue {
  modals: NewModalEntry[];
  /** Push a modal onto the stack. Returns its id. */
  openModal: (config: NewModalConfig) => string;
  /** Close by id, or pop topmost if no id given. */
  closeModal: (id?: string) => void;
  /** Close all modals in the stack. */
  closeAllModals: () => void;
}
