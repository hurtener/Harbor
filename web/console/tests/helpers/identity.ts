// Harbor Console e2e harness — identity helpers (Phase 75 / D-115).
//
// The Harbor isolation contract (CLAUDE.md §6) pins identity to the triple
// `(tenant_id, user_id, session_id)`. Every Console Protocol call carries it.
// These helpers construct a deterministic test identity the harness seeds
// into the per-run Runtime fixture, so per-page specs assert against a known
// tenant/user/session rather than whatever a live environment happens to hold.

/**
 * The Harbor isolation triple. Mirrors the Go-side `identity.Identity` shape;
 * the wire form is generated into `web/console/src/lib/protocol.ts` (D-093).
 */
export type IdentityTriple = {
  tenant: string;
  user: string;
  session: string;
};

/**
 * The deterministic identity the harness seeds by default. Per-page specs may
 * override any component via `makeTestIdentity` to exercise cross-session or
 * cross-tenant isolation.
 */
export const DEFAULT_TEST_IDENTITY: IdentityTriple = {
  tenant: "harbor-e2e-tenant",
  user: "harbor-e2e-user",
  session: "harbor-e2e-session",
};

/**
 * Build a test identity triple, overriding any component of the default.
 * Use distinct triples within one spec to assert isolation (one tenant's
 * data must never leak into another's view).
 */
export function makeTestIdentity(
  overrides: Partial<IdentityTriple> = {},
): IdentityTriple {
  return { ...DEFAULT_TEST_IDENTITY, ...overrides };
}

/**
 * Render an identity triple as a stable cache key — useful for asserting two
 * specs (or two fixture workers) did not collide on the same scope.
 */
export function identityKey(triple: IdentityTriple): string {
  return `${triple.tenant}/${triple.user}/${triple.session}`;
}
