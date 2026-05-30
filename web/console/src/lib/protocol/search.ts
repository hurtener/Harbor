// Harbor Console — `search.*` wire types (Phase 108b chrome).
//
// The top-bar ⌘K global-search launcher consumes the SHIPPED `search.query`
// method (Phase 72c). No new Protocol method is minted here — the launcher is
// a pure consumer (CLAUDE.md §13). These types mirror
// `internal/protocol/types/search.go` (the RPC request/response bodies use
// snake_case json tags — PAGE-POLISH-PROCEDURE §3.3). The Runtime mounts
// `search.query` on the CONTROL surface: `POST /v1/control/search.query`
// (verified live 2026-05-30; `/v1/search/query` is NOT mounted).

/** The four runtime-side indexes `search.query` fans out to. */
export type SearchIndex = 'sessions' | 'tasks' | 'events' | 'artifacts';

/** Optional scope-narrowing filter (within the caller's verified identity). */
export interface SearchFilter {
	tenant_id?: string;
	user_id?: string;
	session_id?: string;
}

/** The `search.query` request body (snake_case — RPC wire shape). */
export interface SearchRequest {
	/** Free-text query; empty lists everything in scope subject to filters. */
	query?: string;
	/** Which indexes to fan out to; empty / omitted means all four. */
	indexes?: SearchIndex[];
	/** Optional scope-narrowing filter. */
	filter?: SearchFilter;
	/** 1-based page number; defaults to 1. */
	page?: number;
	/** Per-page row count; defaults to 20, max 200. */
	page_size?: number;
}

/** A by-reference heavy-content row (D-026) — populated when Preview is heavy. */
export interface SearchArtifactRef {
	id: string;
	mime_type?: string;
	size_bytes?: number;
	filename?: string;
	sha256?: string;
}

/** One uniform result row across all five `search.*` methods. */
export interface SearchResultRow {
	/** Which index produced the row. */
	index: SearchIndex;
	/** The entity id (session / task / event (`<session>:<seq>`) / artifact). */
	id: string;
	tenant_id: string;
	user_id: string;
	session_id: string;
	run_id?: string;
	/** Anchor timestamp (RFC3339). */
	occurred_at?: string;
	/** Redacted short summary; empty when `ref` is populated. */
	preview?: string;
	/** Populated when the preview would exceed the heavy-content threshold. */
	ref?: SearchArtifactRef;
	/** Per-index dimension values (e.g. `{status: "running"}` for tasks). */
	facets?: Record<string, string>;
}

/** The uniform `search.*` response. */
export interface SearchResponse {
	rows: SearchResultRow[];
	page: number;
	page_size: number;
	page_count: number;
	total_count: number;
	has_more: boolean;
	protocol_version?: string;
}
