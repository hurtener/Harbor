// Host-identity + theme injection tests for the MCP Apps host wrapper.
//
// D-091 module-encapsulation hardening: the host identity (`ui/initialize`
// name/version) and theme are injected through `AppBridgeHostOptions`, not
// baked into the module — so a second framework surface mounting the chat
// module advertises its own identity/theme. These tests mock the official
// `AppBridge` to capture its constructor arguments and assert (1) the default
// preserves the prior Console behaviour and (2) an injected hostInfo/theme
// actually flows through to the bridge.

import { describe, expect, it, vi } from 'vitest';

import type { MCPAppHostClient } from './app-bridge-host.js';

// Capture every `new AppBridge(...)` argument list. `vi.hoisted` so the mock
// factory (hoisted above the imports) can close over it.
const captured = vi.hoisted(() => ({ ctorArgs: [] as unknown[][] }));

vi.mock('@modelcontextprotocol/ext-apps/app-bridge', () => {
  class AppBridge {
    oncalltool: unknown;
    onreadresource: unknown;
    onlistresources: unknown;
    onrequestdisplaymode: unknown;
    oninitialized: unknown;
    constructor(...args: unknown[]) {
      captured.ctorArgs.push(args);
    }
    async connect(): Promise<void> {}
    async close(): Promise<void> {}
  }
  class PostMessageTransport {
    constructor(..._args: unknown[]) {}
  }
  return { AppBridge, PostMessageTransport };
});

// Imported AFTER the mock declaration; vitest hoists vi.mock above this.
const { AppBridgeHost, DEFAULT_HOST_INFO } = await import('./app-bridge-host.js');

function fakeClient(): MCPAppHostClient {
  return {
    async readResource(_s, uri) {
      return { resourceUri: uri, mimeType: 'text/html', content: '' };
    },
    async callTool(tool) {
      return { tool, content: {}, isError: false };
    },
    async listResources() {
      return [];
    },
    async listTools() {
      return [];
    },
    async resolveArtifact(id) {
      return `blob:${id}`;
    },
  };
}

describe('AppBridgeHost host-identity + theme injection', () => {
  it('defaults to the Console host identity and dark theme when not injected', () => {
    captured.ctorArgs.length = 0;
    new AppBridgeHost({ client: fakeClient(), serverID: 'srv' });
    const [, hostInfo, , extra] = captured.ctorArgs[0] as [
      null,
      { name: string; version: string },
      unknown,
      { hostContext: { theme: string } },
    ];
    expect(hostInfo).toEqual(DEFAULT_HOST_INFO);
    expect(hostInfo.name).toBe('harbor-console');
    expect(extra.hostContext.theme).toBe('dark');
  });

  it('advertises the INJECTED host identity and theme through the seam', () => {
    captured.ctorArgs.length = 0;
    const hostInfo = { name: 'harbor-dev-ui', version: '9.9' };
    new AppBridgeHost({ client: fakeClient(), serverID: 'srv', hostInfo, theme: 'light' });
    const [, advertised, , extra] = captured.ctorArgs[0] as [
      null,
      { name: string; version: string },
      unknown,
      { hostContext: { theme: string } },
    ];
    expect(advertised).toEqual(hostInfo);
    expect(advertised.name).not.toBe('harbor-console');
    expect(extra.hostContext.theme).toBe('light');
  });
});
