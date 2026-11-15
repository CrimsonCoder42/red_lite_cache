/**
 * shared/modals -- the reference admin's modal system, trimmed to what features/ui/Modal.tsx uses:
 * one portal and stack (ModalProvider), the dialog chrome (BaseModal), the types.
 */
export type { ModalSize, BaseModalProps, NewModalConfig, NewModalEntry, NewModalContextValue } from './types';
export { default as BaseModal } from './BaseModal';
export { NewModalProvider, useNewModalContext } from './ModalProvider';
