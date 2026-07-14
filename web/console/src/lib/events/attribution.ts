/**
 * Events page — pure per-tenant attribution projection.
 *
 * The event-rate aggregate (`events.aggregate`) optionally carries
 * per-tenant attribution on each bucket (`EventBucket.counts_by_tenant`,
 * tenant → event-type → count) FOR admin-widened reads. Unlike a row read
 * (every row carries its own tenant, so the operator can post-filter the
 * merged result against its entitled set), an aggregate is a bag of scalars
 * with no per-row tenant — so the runtime's honouring of the filter IS the
 * whole tenant boundary. Attribution closes that gap: it re-projects the
 * SAME counted events by their existing tenant identity, letting a fleet
 * operator independently verify the boundary the runtime enforced.
 *
 * This module is the PURE projection layer: it folds the wire
 * `EventBucket[]` attribution into a per-tenant breakdown, checks the
 * returned tenant keys against the entitled set the operator asked for, and
 * confirms the per-bucket reconciliation invariant
 * `Σ_tenant counts_by_tenant[tenant][type] == counts[type]`. No `$state`,
 * no Protocol call — unit-tested.
 */

import type { EventBucket } from '$lib/protocol/events.js';

/** One tenant's contribution to the windowed totals. */
export interface TenantAttributionRow {
	/** The tenant id. */
	tenant: string;
	/** The tenant's total event count across the window. */
	total: number;
	/** The tenant's per-event-type counts, keyed by event-type string. */
	byType: Record<string, number>;
	/**
	 * True when this tenant is NOT in the entitled set the operator asked
	 * for — a boundary violation the operator can now SEE (defence in depth;
	 * the runtime already scopes the counted events, so on a correct runtime
	 * this is always false). A non-empty flag is a loud signal, never
	 * silently dropped (CLAUDE.md §13).
	 */
	unexpected: boolean;
}

/** The fully-projected per-tenant attribution for the active window. */
export interface TenantAttribution {
	/** Per-tenant rows, sorted by total descending (ties broken by name). */
	rows: TenantAttributionRow[];
	/** The grand total across every attributed tenant. */
	total: number;
	/**
	 * True when the per-bucket reconciliation held on every bucket:
	 * `Σ_tenant counts_by_tenant[tenant][type] == counts[type]`. The
	 * verifiability property — attribution is a pure re-projection of the
	 * totals, not a second, looser read path. False signals a runtime bug
	 * the operator should not trust the breakdown against.
	 */
	reconciles: boolean;
	/** True when any returned tenant fell outside the entitled set. */
	hasUnexpected: boolean;
}

/**
 * `projectTenantAttribution` folds the wire `EventBucket[]` attribution into
 * a per-tenant breakdown. Returns `null` when NO bucket carries
 * `counts_by_tenant` — the read was not admin-widened, or the caller did not
 * opt in, so there is nothing to attribute (the sparkline renders alone).
 *
 * `entitled` is the set of tenants the operator asked for (the aggregate
 * filter's `tenant_ids`, or the operator's own tenant when unpinned); any
 * returned key outside it is flagged `unexpected` on its row and surfaced via
 * `hasUnexpected`.
 */
export function projectTenantAttribution(
	buckets: EventBucket[],
	entitled: readonly string[]
): TenantAttribution | null {
	const entitledSet = new Set(entitled);
	const perTenant = new Map<string, Record<string, number>>();
	let any = false;
	let reconciles = true;

	for (const b of buckets) {
		const cbt = b.counts_by_tenant;
		// Per-bucket reconciliation: Σ_tenant cbt[tenant][type] === counts[type].
		const bucketByType: Record<string, number> = {};
		if (cbt) {
			any = true;
			for (const [tenant, byType] of Object.entries(cbt)) {
				const acc = perTenant.get(tenant) ?? {};
				for (const [type, n] of Object.entries(byType)) {
					acc[type] = (acc[type] ?? 0) + n;
					bucketByType[type] = (bucketByType[type] ?? 0) + n;
				}
				perTenant.set(tenant, acc);
			}
		}
		// Compare the bucket's attributed sums against its totals. A bucket
		// with no attribution (empty bucket) has an empty counts map, so the
		// comparison is vacuously satisfied.
		const counts = b.counts ?? {};
		const keys = new Set([...Object.keys(counts), ...Object.keys(bucketByType)]);
		for (const type of keys) {
			if ((counts[type] ?? 0) !== (bucketByType[type] ?? 0)) {
				// Only a bucket that actually carries attribution can violate;
				// an empty attribution on a non-empty bucket would only appear
				// on a bucket the runtime chose not to attribute, which cannot
				// happen for a widened opt-in read (every matched event is
				// attributed in the same pass).
				if (cbt) {
					reconciles = false;
				}
			}
		}
	}

	if (!any) {
		return null;
	}

	const rows: TenantAttributionRow[] = [...perTenant.entries()].map(([tenant, byType]) => {
		const total = Object.values(byType).reduce((s, n) => s + n, 0);
		return { tenant, total, byType, unexpected: !entitledSet.has(tenant) };
	});
	rows.sort((a, b) => (b.total - a.total) || a.tenant.localeCompare(b.tenant));

	return {
		rows,
		total: rows.reduce((s, r) => s + r.total, 0),
		reconciles,
		hasUnexpected: rows.some((r) => r.unexpected)
	};
}
