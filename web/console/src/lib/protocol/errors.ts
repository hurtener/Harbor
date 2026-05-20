// Harbor Console — the ONE Protocol error class (D-121, CONVENTIONS.md §6/§8).
//
// The foundation audit found five legacy hand-authored clients each carrying
// their own error class (`ToolsProtocolError`, `MemoryProtocolError`,
// `FlowsClientError`, `ProtocolCallError`, `ProtocolRequestError`) with
// inconsistent shapes — one of them silently dropped the HTTP status. This
// module ships the single canonical error every `HarborClient` method rejects
// with. Uniform `(code, message, status)`; status is NEVER dropped.

/** The canonical Protocol error envelope `{code, message}` the Runtime returns. */
export interface ProtocolErrorBody {
	code: string;
	message: string;
}

/**
 * The single error class raised by `HarborClient` on any non-2xx Runtime
 * response. A page catches a `ProtocolError` and routes it into `PageState`'s
 * Error state, which renders `code: message` + a Retry button (CONVENTIONS.md
 * §4/§8). Carrying the canonical `code` lets a page branch on
 * `identity_required` / `scope_mismatch` / `not_found` without string-matching
 * the human message; carrying `status` lets it distinguish 401 / 403 / 404 /
 * 413 / 501. Neither field is ever dropped.
 */
export class ProtocolError extends Error {
	/** The canonical Protocol error code (e.g. `identity_scope_required`). */
	readonly code: string;
	/** The HTTP status the Runtime returned. */
	readonly status: number;

	constructor(code: string, message: string, status: number) {
		super(message || code);
		this.name = 'ProtocolError';
		this.code = code;
		this.status = status;
	}
}
