// Harbor Console — typed State-snapshots Protocol surface (D-254).
//
// These hand-maintained wire types mirror `internal/protocol/types/state.go`
// (the `state.history` request/response + the flat routable artifact ref +
// the flat event projection). They are kept in lockstep with the Go single
// source by `make protocol-ts-gen-check` (the field-level manifest gate,
// D-223). A Go-side change to any of these shapes MUST be mirrored here by
// hand and the manifest regenerated.

/** Default + maximum window size for `state.history`, mirroring the Go bounds. */
export const DEFAULT_STATE_HISTORY_LIMIT = 50;
export const MAX_STATE_HISTORY_LIMIT = 200;

/**
 * The `state.history` request — a by-id, bounded, tail-first backward
 * window read of a session's durable event stream. Mirrors
 * `types.StateHistoryRequest`. The `identity` triple is folded into the
 * body by the shared HarborClient transport — callers omit it.
 */
export interface StateHistoryRequest {
	session_id: string;
	/** Exclusive upper bound: only events with sequence < before. Zero ⇒ from the tail. */
	before?: number;
	/** Window size K (clamped to MAX_STATE_HISTORY_LIMIT; zero ⇒ default). */
	limit?: number;
}

/**
 * The flat, routable artifact reference a replayed heavy-payload event
 * carries — mirrors `types.StateArtifactRef`. The `id` routes to
 * `artifacts.get_ref` (presigned URL on an S3-compat store, or the typed
 * `presign_unsupported` on the default inmem/fs stores).
 */
export interface StateArtifactRef {
	id: string;
	mime_type?: string;
	size_bytes?: number;
	filename?: string;
	sha256?: string;
}

/**
 * The flat wire projection of one durable event — mirrors
 * `types.StateEvent`. The client reduces a page of these into chat
 * messages (the reducer the Console already owns).
 */
export interface StateEvent {
	type: string;
	sequence: number;
	occurred_at: string;
	tenant: string;
	user: string;
	session: string;
	run?: string;
	payload?: unknown;
	extra?: Record<string, string>;
	artifacts?: StateArtifactRef[];
}

/**
 * The `state.history` response — a bounded page of flat events oldest-first
 * within the window, plus the head/tail bounds, a scroll-up cursor, and the
 * honest retention-gap signal. Mirrors `types.StateHistoryResponse`.
 */
export interface StateHistoryResponse {
	events: StateEvent[];
	head_sequence: number;
	tail_sequence: number;
	/** The lowest sequence in this page — pass back as `before` to scroll older. Zero at head. */
	next_cursor: number;
	has_more: boolean;
	truncated?: boolean;
}
