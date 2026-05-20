// Harbor Console — shared UI component inventory (D-121,
// CONVENTIONS.md §3).
//
// The cross-page component inventory. Every Console page composes its
// surface from these primitives; a page never forks a primitive that
// already lives here. Page-specific components stay in `components/<page>/`.

export { default as PageHeader } from './PageHeader.svelte';
export { default as FilterBar } from './FilterBar.svelte';
export { default as SavedViewChips, type SavedView } from './SavedViewChips.svelte';
export { default as DataTable, type DataTableColumn } from './DataTable.svelte';
export { default as BulkActionBar } from './BulkActionBar.svelte';
export { default as DetailRail } from './DetailRail.svelte';
export { default as RailCard } from './RailCard.svelte';
export { default as StatusChip, type StatusKind } from './StatusChip.svelte';
export { default as Pagination } from './Pagination.svelte';
export { default as ConnectionFooter } from './ConnectionFooter.svelte';
export { default as PageState, type PageStatus } from './PageState.svelte';
