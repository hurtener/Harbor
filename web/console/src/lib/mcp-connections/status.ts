// MCP server-state → shared StatusChip kind mapping (D-121, MCP refactor).
//
// The legacy page hand-rolled five `chip-<state>` CSS classes off the
// `--color-mcp-*` token aliases. The D-121 refactor replaces those with
// the shared `<StatusChip>`, whose `kind` prop maps over the canonical
// status token scale (CONVENTIONS.md §3). This module is the page-local
// translation table from the MCP wire `state` enum onto that scale.

import type { StatusKind } from '$lib/components/ui/StatusChip.svelte';
import type { MCPServerState } from '$lib/protocol/mcp.js';

/** Maps an MCP server `state` onto the shared `StatusChip` kind scale. */
export function mcpStatusKind(state: MCPServerState): StatusKind {
  switch (state) {
    case 'online':
      return 'success';
    case 'reconnecting':
      return 'warning';
    case 'auth_pending':
      return 'accent';
    case 'error':
      return 'danger';
    case 'offline':
    default:
      return 'neutral';
  }
}

/** A human-facing label for an MCP server `state`. */
export function mcpStateLabel(state: MCPServerState): string {
  switch (state) {
    case 'online':
      return 'Online';
    case 'reconnecting':
      return 'Reconnecting';
    case 'offline':
      return 'Offline';
    case 'auth_pending':
      return 'Auth pending';
    case 'error':
      return 'Errored';
    default:
      return state;
  }
}
