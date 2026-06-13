// Phase 109b — the inline MCP App renderer registers on the canonical
// renderer registry (D-091 chat-module registry) under its synthetic MIME.

import { describe, expect, it } from 'vitest';

import { dispatchRenderer, MCP_APP_INLINE_MIME, registeredSources } from './index.js';

describe('MCP App inline renderer registration', () => {
  it('dispatches the synthetic MCP-App MIME to the mcp-app renderer', () => {
    expect(dispatchRenderer(MCP_APP_INLINE_MIME).source).toBe('mcp-app');
  });

  it('does not hijack a real text/html artifact', () => {
    expect(dispatchRenderer('text/html').source).not.toBe('mcp-app');
  });

  it('registers the mcp-app source in the registry', () => {
    expect(registeredSources()).toContain('mcp-app');
  });
});
