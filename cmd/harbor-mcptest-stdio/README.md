# harbor-mcptest-stdio

A minimal MCP stdio server used by the Phase 83g integration test
(`test/integration/phase83g_mcp_dev_consumer_test.go`). Exposes one
tool — `echo` — and nothing else.

**Test fixture only.** Not shipped in releases; not referenced by any
operator path. Built by the integration test via `go build` into a
tempdir. The binary is the canonical reference for "minimal MCP stdio
server" against which Harbor's dev-binary MCP consumer wiring (Phase
83g — D-150) is exercised end-to-end.

If you're an operator looking for a real stdio MCP server, this is not
it. See `examples/harbor.yaml` for the `mcp_servers[]` block that
points at external stdio servers (e.g. via `uvx <package>`).
