// Settings page — load function (Phase 73m / D-129).
//
// The Console is a client-side SPA (D-091) — no SSR. The Settings
// page's connection + identity + scope are resolved client-side from
// `$lib/connection.ts` inside `+page.svelte`'s `onMount`, NOT in a
// server `load`. This module exists only to pin the no-SSR posture for
// the route (CONVENTIONS.md §1, CLAUDE.md §4.5 rule 7).

export const ssr = false;
export const prerender = false;
