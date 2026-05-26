// Phase 105 (V1.2) — AttachToLocalCard bootstrap-fetch flow.
//
// Extracted from AttachToLocalCard.svelte so the network/branching logic
// is unit-testable (matching the repo's existing
// state.svelte.ts / activity.ts pattern). The Svelte component is a thin
// renderer around `runAttachToLocal`.

/** Shape of the bootstrap envelope the Runtime returns. */
export interface BootstrapEnvelope {
	base_url: string;
	token: string;
	identity: { tenant: string; user: string; session: string };
	scopes: string[];
	protocol_version?: string;
}

/** Outcome of one attach attempt — drives the card's status surface. */
export type AttachOutcome =
	| { kind: 'attached'; envelope: BootstrapEnvelope }
	| { kind: 'info'; message: string }
	| { kind: 'error'; message: string };

/** Deps the flow needs — kept narrow so tests can substitute trivially. */
export interface AttachDeps {
	fetch: typeof globalThis.fetch;
	origin: string;
}

/**
 * runAttachToLocal calls the bootstrap endpoint at `${origin}/v1/dev/bootstrap.json`
 * and returns a typed outcome. Per AC-9:
 *   - 200 → attached (caller will `attachConnection(...)` + reload).
 *   - 403 → info banner "endpoint not available; use the manual form".
 *   - 404 → info banner "endpoint not registered on this build".
 *   - other status → error banner with the status code.
 *   - thrown fetch error (network) → error banner with the message.
 */
export async function runAttachToLocal(deps: AttachDeps): Promise<AttachOutcome> {
	try {
		const resp = await deps.fetch(`${deps.origin}/v1/dev/bootstrap.json`, {
			method: 'POST',
			credentials: 'omit'
		});
		if (resp.ok) {
			const envelope = (await resp.json()) as BootstrapEnvelope;
			return { kind: 'attached', envelope };
		}
		if (resp.status === 403) {
			return {
				kind: 'info',
				message: 'Local-bootstrap endpoint not available; use the manual form below.'
			};
		}
		if (resp.status === 404) {
			return {
				kind: 'info',
				message:
					'Local-bootstrap endpoint not available on this build; use the manual form below.'
			};
		}
		return { kind: 'error', message: `Bootstrap endpoint returned ${resp.status}.` };
	} catch (e) {
		return {
			kind: 'error',
			message: e instanceof Error ? e.message : 'Network error reaching the bootstrap endpoint.'
		};
	}
}
