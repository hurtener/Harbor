// Harbor Console — Memory-page wire types (D-121, CONVENTIONS.md §6).
//
// The typed wire shapes the `memory.*` namespace methods on `HarborClient`
// return. The Memory-page refactor (D-121) deleted the legacy hand-authored
// `protocol-memory.ts` client class — its `fetch` choke point folded into the
// unified `HarborClient` transport — but the wire TYPES stay: a page that
// calls `client.memory.list<MemoryListResponse>(...)` narrows the generic
// namespace result against these interfaces.
//
// # Single source of truth
//
// Every interface below mirrors a struct in
// `internal/protocol/types/memory.go` (D-002). When `cmd/harbor-gen-protocol-ts`
// (D-093) lands, these types fold into the generated `protocol.ts` and this
// module collapses to a thin re-export — a mechanical migration.

/** Memory-record scope — the wire projection of `types.MemoryScope`. */
export type MemoryScope = 'session' | 'user' | 'tenant';

/** Memory strategy — the wire projection of `types.MemoryStrategyName`. */
export type MemoryStrategyName = 'none' | 'truncation' | 'rolling_summary';

/** Memory driver — the wire projection of `types.MemoryDriverName`. */
export type MemoryDriverName = 'inmem' | 'sqlite' | 'postgres';

/** Default / max `memory.list` page size — mirrors the Go-side constants. */
export const DEFAULT_MEMORY_LIST_PAGE_SIZE = 50;
export const MAX_MEMORY_LIST_PAGE_SIZE = 200;

/** Flat identity scope carried on a wire row — mirrors `types.IdentityScope`. */
export interface IdentityScope {
	tenant: string;
	user: string;
	session: string;
}

/** One Memory-page table row — mirrors `types.MemoryItem`. */
export interface MemoryItem {
	key: string;
	strategy: string;
	scope: string;
	identity: IdentityScope;
	agent_id?: string;
	created_at: string;
	last_updated_at: string;
	expires_at?: string;
	size_bytes: number;
	heavy_content?: boolean;
	driver: string;
}

/** The `memory.list` query filter — mirrors `types.MemoryFilter`. */
export interface MemoryFilter {
	tenant_ids?: string[];
	user_ids?: string[];
	session_ids?: string[];
	agent_ids?: string[];
	scopes?: string[];
	drivers?: string[];
	strategies?: string[];
	has_ttl_expiring?: boolean;
	content_search?: string;
}

/** The `memory.list` request — mirrors `types.MemoryListRequest`. */
export interface MemoryListRequest {
	identity?: IdentityScope;
	filter?: MemoryFilter;
	page?: number;
	page_size?: number;
}

/** Page-level counters — mirrors `types.MemoryAggregates`. */
export interface MemoryAggregates {
	total: number;
	expiring_in_1h: number;
	identity_rejected_24h: number;
	recovery_dropped_24h: number;
}

/** The `memory.list` response — mirrors `types.MemoryListResponse`. */
export interface MemoryListResponse {
	items: MemoryItem[];
	page: number;
	page_size: number;
	page_count: number;
	total_rows: number;
	aggregates: MemoryAggregates;
	protocol_version: string;
}

/** By-reference heavy-value stub — mirrors `types.MemoryArtifactRef`. */
export interface MemoryArtifactRef {
	id: string;
	mime_type?: string;
	size_bytes?: number;
	filename?: string;
	sha256?: string;
}

/** Per-record metadata — mirrors `types.MemoryMetadata`. */
export interface MemoryMetadata {
	ttl?: number;
	strategy_config?: Record<string, string>;
	related_event_ids?: string[];
}

/** The `memory.get` request — mirrors `types.MemoryGetRequest`. */
export interface MemoryGetRequest {
	identity?: IdentityScope;
	key: string;
}

/** A single record's full detail — mirrors `types.MemoryItemDetail`. */
export interface MemoryItemDetail {
	item: MemoryItem;
	/** Post-redaction value, populated ONLY below the heavy threshold (D-026). */
	value?: string;
	/** By-reference stub, populated ONLY at/above the heavy threshold (D-026). */
	value_artifact?: MemoryArtifactRef;
	metadata: MemoryMetadata;
}

/** The `memory.get` response — mirrors `types.MemoryGetResponse`. */
export interface MemoryGetResponse {
	detail: MemoryItemDetail;
	protocol_version: string;
}

/** Aggregate health counters — mirrors `types.MemoryHealthAggregate`. */
export interface MemoryHealthAggregate {
	total: number;
	expiring_in_1h: number;
	identity_rejected_24h: number;
	recovery_dropped_24h: number;
	driver_by_scope: Record<string, string>;
}

/** The `memory.health` response — mirrors `types.MemoryHealthResponse`. */
export interface MemoryHealthResponse {
	aggregate: MemoryHealthAggregate;
	protocol_version: string;
}
