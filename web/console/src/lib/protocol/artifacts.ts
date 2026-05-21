/**
 * Artifacts-page Protocol wire types (Phase 73l / D-120; built against
 * D-121). Moved out of the legacy `$lib/protocol.ts` stub in the Wave 13
 * §17.5 checkpoint (D-132 / W6) so the artifacts page imports its wire
 * types from the same `$lib/protocol/<page>.ts` location every other
 * page does (Tools / Memory / MCP / Flows / Sessions).
 *
 * The wire shapes mirror `internal/protocol/types/artifacts.go` field-
 * for-field (the Go-side single source per D-002). When the
 * `cmd/harbor-gen-protocol-ts` generator (D-093) ships, these types fold
 * into the generated client surface and this module re-exports from
 * there — a mechanical migration (tracked: D-132 generator follow-up).
 *
 * Heavy artifact bytes flow by REFERENCE (D-026): a list / get_ref
 * response never carries inline bytes. `artifacts.put` carries base64
 * bytes on the request leg only.
 */

/**
 * The flat wire identity an artifacts-method request carries. Mirrors
 * the Go `internal/protocol/types.ArtifactScope` — `(tenant, user,
 * session, task)`. Identity is mandatory: tenant/user/session must be
 * non-empty for put / get_ref. For list, empty fields are wildcards.
 */
export interface ArtifactScope {
  tenant: string;
  user: string;
  session: string;
  task?: string;
}

/** The closed enum of artifact producers. */
export type ArtifactSource = 'tool' | 'planner' | 'user_upload' | 'system';

/** Optional byte-size filter for `artifacts.list`. Both bounds inclusive. */
export interface SizeRange {
  min_bytes?: number;
  max_bytes?: number;
}

/** Optional created-at filter for `artifacts.list`. */
export interface TimeRange {
  after?: string;
  before?: string;
}

/** The flat Protocol projection of the storage-side artifact reference. */
export interface ArtifactRef {
  id: string;
  mime_type?: string;
  size_bytes: number;
  filename?: string;
  sha256?: string;
  namespace?: string;
  scope: ArtifactScope;
}

/** The `artifacts.list` catalog row shape. */
export interface ArtifactRow {
  ref: ArtifactRef;
  tags?: string[];
  source?: ArtifactSource;
  driver?: string;
  created_at?: string;
}

/** The `artifacts.list` request — Scope plus the Phase 73l filter extensions. */
export interface ArtifactsListRequest {
  scope: ArtifactScope;
  mime_type?: string[];
  source?: ArtifactSource[];
  size_range?: SizeRange;
  created_range?: TimeRange;
  tags?: string[];
  limit?: number;
}

/** The `artifacts.list` response — metadata-only rows (D-026). */
export interface ArtifactsListResponse {
  rows: ArtifactRow[];
  total_matched: number;
  protocol_version: string;
}

/** Optional upload metadata for `artifacts.put`. */
export interface ArtifactsPutOpts {
  mime_type?: string;
  filename?: string;
  namespace?: string;
  source?: ArtifactSource;
  tags?: string[];
}

/** The `artifacts.put` request — upload bytes (base64) on the request leg only. */
export interface ArtifactsPutRequest {
  scope: ArtifactScope;
  /** Base64-encoded artifact bytes (Go `[]byte` JSON encoding). */
  bytes: string;
  opts?: ArtifactsPutOpts;
}

/** The `artifacts.put` response — the minted reference, never the bytes. */
export interface ArtifactsPutResponse {
  ref: ArtifactRef;
  protocol_version: string;
}

/** The `artifacts.get_ref` request — the read-side presigned-URL resolver. */
export interface ArtifactsGetRefRequest {
  scope: ArtifactScope;
  id: string;
  /** Expiry in nanoseconds (Go `time.Duration`); bounded [1m, 7d]. */
  expiry?: number;
}

/** The `artifacts.get_ref` response — a time-bounded presigned URL. */
export interface ArtifactsGetRefResponse {
  ref: ArtifactRef;
  presigned_url: string;
  expires_at: string;
  protocol_version: string;
}
