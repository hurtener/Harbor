// Phase 105 (V1.2) — Connected Runtimes "Add Runtime" form validation.
//
// Extracted from ConnectedRuntimesCard.svelte so the validation logic
// is unit-testable in Vitest without a Svelte renderer (the repo's
// existing convention — see web/console/src/lib/mcp-connections/state.svelte.ts
// and web/console/src/lib/overview/activity.ts).

/** AC-1 token shape: three base64url segments separated by '.'. */
const JWT_LIKE_RE = /^[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+$/;

/** Minimum JWT-like token length the form accepts. */
export const MIN_TOKEN_LENGTH = 200;

/** Raw inputs the form binds to. All strings; trim happens in the validator. */
export interface AddRuntimeDraft {
	name: string;
	url: string;
	token: string;
	tenant: string;
	user: string;
	session: string;
}

/**
 * validateAddRuntimeDraft returns the first violation as a user-facing
 * error string, or null when the draft is acceptable. The order matches
 * the form's HTML field order so error text consistently references the
 * first empty / invalid field the operator can see.
 */
export function validateAddRuntimeDraft(draft: AddRuntimeDraft): string | null {
	if (draft.name.trim() === '') return 'Name is required.';
	if (draft.url.trim() === '') return 'Base URL is required.';
	try {
		new URL(draft.url.trim());
	} catch {
		return 'Base URL is not a valid URL.';
	}
	if (draft.token.trim() === '') return 'Token is required.';
	if (draft.token.trim().length < MIN_TOKEN_LENGTH) {
		return 'Token is too short (JWT-like token expected).';
	}
	if (!JWT_LIKE_RE.test(draft.token.trim())) {
		return 'Token does not look like a JWT (expected three base64url segments separated by dots).';
	}
	if (draft.tenant.trim() === '') return 'Tenant is required.';
	if (/\s/.test(draft.tenant)) return 'Tenant must not contain whitespace.';
	if (draft.user.trim() === '') return 'User is required.';
	if (/\s/.test(draft.user)) return 'User must not contain whitespace.';
	if (draft.session.trim() === '') return 'Session is required.';
	if (/\s/.test(draft.session)) return 'Session must not contain whitespace.';
	return null;
}
