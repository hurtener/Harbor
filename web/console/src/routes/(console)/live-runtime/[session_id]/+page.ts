// Live Runtime session-scoped route — load function (Phase 73b /
// D-126).
//
// The Console is a client-side SPA (D-091) — no SSR. The session-scoped
// Live Runtime route (`/live-runtime/<session_id>`) is the deep-link
// target; the page resolves its connection client-side in `onMount`.
// This module pins the no-SSR posture for the route (CONVENTIONS.md §1,
// CLAUDE.md §4.5 rule 7).

export const ssr = false;
export const prerender = false;
