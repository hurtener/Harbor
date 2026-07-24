// Test-only harness: mount the real `McpAppRenderer` with REACTIVE props so a
// spec can churn a prop's identity after mount and observe whether the renderer
// re-runs its bridge lifecycle. Svelte 5 propagates prop changes to a mounted
// component only when the props object is a `$state` proxy — and runes are only
// available in `.svelte` / `.svelte.ts` files, so this thin holder lives here
// and is imported by `mcp-app-theme-lifecycle.spec.ts`.

import { mount, type Component } from 'svelte';

import McpAppRenderer from './mcp-app.svelte';
import type { RendererProps } from './index.js';

/** A mounted renderer plus the reactive props object the caller mutates. */
export interface ReactiveMount {
  component: ReturnType<typeof mount>;
  /** Mutate a key (e.g. `props.appHostClient = other`) to churn a prop. */
  props: RendererProps;
}

/**
 * Mount `McpAppRenderer` against `target` with a `$state`-backed props object.
 * Reassigning any key on the returned `props` propagates to the live component,
 * so a test can prove that prop-identity churn does NOT re-run the bridge
 * lifecycle effect.
 */
export function mountRendererReactive(target: HTMLElement, initial: RendererProps): ReactiveMount {
  const props = $state(initial);
  const component = mount(McpAppRenderer as Component<RendererProps>, { target, props });
  return { component, props };
}
