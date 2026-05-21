/**
 * Pause-snapshot wire types — the `pause.list` Protocol shapes the
 * Console Overview-page intervention queue consumes (Phase 73a / D-127).
 *
 * # Wire types only — the client lives in `client.ts`
 *
 * This module is the wire-type surface only. It mirrors
 * `internal/protocol/types/pause.go` field-for-field — the Go side is
 * the single source (D-002 / D-093). When `cmd/harbor-gen-protocol-ts`
 * (D-093) ships, these absorb into the generated `protocol.ts`.
 *
 * # No new Protocol method (Phase 73a)
 *
 * Phase 73a ships NO new Protocol method. `pause.list` is already
 * shipped (Phase 72e / D-110) at `POST /v1/pause/list`. The Overview
 * intervention queue is a pure UI consumer of that snapshot surface;
 * the Approve / Reject row actions invoke the SHIPPED Phase 54 `approve`
 * / `reject` control verbs through {@link ControlNamespace} — there is
 * NO parallel implementation (CLAUDE.md §13).
 */

import type { ConnectionIdentity } from '../connection.js';

/** The closed set of pause-snapshot lifecycle states. */
export type PauseSnapshotState = 'paused' | 'resumed';

/** Pause-snapshot pagination defaults — mirror the Go `pause.go` constants. */
export const DEFAULT_PAUSE_LIST_PAGE_SIZE = 50;
/** Pause-snapshot pagination max — mirror the Go `pause.go` constants. */
export const MAX_PAUSE_LIST_PAGE_SIZE = 200;

/**
 * The by-reference payload shape a {@link PauseSnapshot} carries when
 * the pause record's payload met the D-026 heavy-content threshold.
 * Mirrors `types.PauseArtifactRef`.
 */
export interface PauseArtifactRef {
	/** The content-addressed artifact identifier. */
	id: string;
	/** The IANA media type, when known. */
	mime_type?: string;
	/** The length of the referenced bytes. */
	size_bytes?: number;
	/** Metadata-only filename (never used for path construction). */
	filename?: string;
	/** The full hex digest of the referenced bytes. */
	sha256?: string;
}

/**
 * One Coordinator pause record, projected for the wire. Mirrors
 * `types.PauseSnapshot`. Exactly one of `payload` / `payload_ref` is
 * populated for a pause carrying a payload (D-026).
 */
export interface PauseSnapshot {
	/** The opaque runtime-issued pause token — the resume/approve/reject handle. */
	token: string;
	/** One of the four canonical pause reasons (RFC §6.3). */
	reason: string;
	/** The lifecycle state — `paused` or `resumed`. */
	state: PauseSnapshotState;
	/** The (tenant, user, session [, run]) the pause was recorded under. */
	identity: ConnectionIdentity & { run?: string };
	/** The wall-clock time the pause was recorded (RFC-3339 UTC). */
	paused_at: string;
	/** The wall-clock time Resume was called — omitted unless `state === 'resumed'`. */
	resumed_at?: string;
	/** The inline sanitised payload, when below the heavy-content threshold. */
	payload?: Record<string, unknown>;
	/** The by-reference payload, when it exceeded the heavy-content threshold. */
	payload_ref?: PauseArtifactRef;
}

/**
 * Narrows a `pause.list` response. An empty filter means "the caller's
 * own identity scope, status=paused" — the intervention-queue default.
 * A `tenant_ids` value reaching outside the caller's own tenant (or
 * naming more than one tenant) requires `auth.ScopeAdmin` (D-079).
 * Mirrors `types.PauseFilter`.
 */
export interface PauseFilter {
	/** Lifecycle-state filter; empty defaults to `["paused"]`. */
	status?: string[];
	/** Tenant axis — a foreign tenant or len>1 requires admin. */
	tenant_ids?: string[];
	/** User axis. */
	user_ids?: string[];
	/** Session axis. */
	session_ids?: string[];
	/** Run axis. */
	run_ids?: string[];
	/** Reason axis — one or more canonical pause reasons. */
	reasons?: string[];
	/** Optional inclusive lower bound on `paused_at`. */
	since?: string;
	/** Optional inclusive upper bound on `paused_at`. */
	until?: string;
}

/** The `pause.list` request shape. Mirrors `types.PauseListRequest`. */
export interface PauseListRequest {
	/** Narrows the snapshot; empty = caller's own scope, status=paused. */
	filter?: PauseFilter;
	/** 1-based page number; defaults to 1. */
	page?: number;
	/** Per-page row count; defaults to {@link DEFAULT_PAUSE_LIST_PAGE_SIZE}. */
	page_size?: number;
}

/** The `pause.list` response shape. Mirrors `types.PauseListResponse`. */
export interface PauseListResponse {
	/** The page of pause records, newest-first (`paused_at` descending). */
	snapshots: PauseSnapshot[];
	/** The 1-based page number this response covers. */
	page: number;
	/** The per-page row count applied. */
	page_size: number;
	/** The total number of pages over the filtered set. */
	page_count: number;
	/** The total filtered row count across all pages. */
	total_rows: number;
	/** True when a `status=resumed` filter aged out beyond registry retention. */
	truncated?: boolean;
}
