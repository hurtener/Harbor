// Package devdraft implements the `harbor dev` draft-save scaffolding
// surface — Phase 66 / D-100.
//
// # What this package owns
//
// A project-local `.harbor/drafts/<tenant>/<user>/<session>/<draft_id>/`
// scratchpad where an operator iterates on an agent without committing
// scaffold output. The Store materialises the same file tree
// `harbor scaffold` emits (agent.go, agent_test.go, harbor.yaml,
// README.md, go.mod) and exposes a small HTTP surface mounted by
// `harbor dev` under `/v1/dev/drafts/`. A `save` promotes a draft to a
// `harbor scaffold`-emitted layout under an operator-supplied output
// dir; before promotion the rendered `harbor.yaml` is run through
// `internal/config.Load + Validate` so an invalid draft cannot leak
// into the operator's working tree.
//
// # Identity scoping (CLAUDE.md §6)
//
// The on-disk path is identity-scoped — the tuple `(tenant, user,
// session)` is the prefix. Concurrent operators (multiple `harbor
// dev` clients hitting the same `.harbor/drafts/` root) cannot
// collide. The `draft_id` is an opaque ULID; the Store's APIs always
// take an `identity.Identity` from `ctx` and reject any request whose
// identity is missing or incomplete (§6 rule 9: identity is mandatory;
// fail closed). Cross-identity reads are impossible at the
// filesystem layer because the path is composed from the identity
// before any file open.
//
// # Path-traversal safety (CLAUDE.md §7 rule 5)
//
// Every operator-supplied path component (the `{path}` in
// `PATCH /v1/dev/drafts/{id}/files/{path}` and the operator-supplied
// output dir on `save`) is filtered through the local `resolveSafe`
// helper which mirrors `internal/skills/importer/path_safety.go`:
// `filepath.Clean` + lexical-prefix verification + a symlink-eval
// pass. Escape attempts fail loud with ErrUnsafePath.
//
// # Bus events
//
// Five lifecycle events land on the canonical event bus so the
// Console (and integration tests) observe the round-trip:
//
//   - `dev.draft.created`
//   - `dev.draft.updated`
//   - `dev.draft.previewed`
//   - `dev.draft.saved`
//   - `dev.draft.discarded`
//
// All five are SafePayload by construction — the payload carries the
// draft ID and a short marker (file path, output dir abs path) and
// never the file contents.
//
// # Concurrent reuse (D-025)
//
// The Store is a compiled artifact: every field is set at
// construction and immutable afterwards; per-request state lives in
// `ctx`. The concurrent-reuse test exercises N≥100 invocations
// against a single shared Store under `-race`.
package devdraft
