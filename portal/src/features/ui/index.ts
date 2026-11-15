/**
 * features/ui -- the small, reusable pieces the rebuilt admin is assembled from.
 *
 * Nine files, each one file, each under 150 lines, none of them holding
 * page-specific logic. The thing this folder exists to prevent is on record in
 * doc/admin/rebuild.md § Never repeat.
 *
 * Nothing speculative lives here. The gaps the template did not cover (toasts,
 * date pickers, file upload, tabs, confirm dialogs) get a file when a stage
 * needs one -- react-toastify and shared/modals already cover two of them.
 */

export { Badge, type BadgeProps, type BadgeTone } from './Badge';
export {
  Avatar,
  BoolBadge,
  DateCell,
  IdentityCell,
  Mono,
  Muted,
  Primary,
  TagBadge,
  Truncated,
} from './cells';
export { FilterTabs, type FilterTab, type FilterTabsProps } from './FilterTabs';
export { RowMenu, type RowMenuItem, type RowMenuProps } from './RowMenu';
export { SearchInput, type SearchInputProps } from './SearchInput';
export { Button, type ButtonProps, type ButtonSize, type ButtonVariant } from './Button';
export {
  DataTable,
  type Column,
  type DataTableProps,
  type PaginationState,
  type SortState,
} from './DataTable';
export { EmptyState, type EmptyStateProps } from './EmptyState';
export { GlassCard, type GlassCardProps } from './GlassCard';
export { Input, type InputProps } from './Input';
export { useModal, type ModalOptions, type UseModal } from './Modal';
export { Skeleton, SkeletonRows, type SkeletonProps } from './Skeleton';
export { StatTile, type StatTileProps, type StatTone } from './StatTile';
export { TablePager, type TablePagerProps } from './TablePager';
