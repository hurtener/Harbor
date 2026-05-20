// The Harbor Console is a client-side SPA (D-091) — no SSR, no prerender of
// dynamic routes. SvelteKit emits a static fallback shell via adapter-static.
export const ssr = false;
export const prerender = false;
