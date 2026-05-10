# Vendored protocol specifications

External wire-protocol specifications Harbor consumes are mirrored
into this directory so the build is reproducible and the source-of-
truth is searchable from inside the repo. Each file carries a
header comment naming the upstream URL + commit SHA at the time of
vendoring.

## Inventory

| File | Upstream | Pinned commit | Phase |
|------|----------|----------------|-------|
| `a2a.proto` | `github.com/a2aproject/A2A/blob/main/specification/a2a.proto` | `ae6a562d5d972f2c4b184f748bb32e1fa9aa7bf2` (2026-04-23) | Phase 22 (RemoteTransport contracts) + Phase 29 (A2A southbound) |

## Bumping a vendored spec

When the upstream advances and Harbor wants to track:

1. Open the target file's upstream URL at the new commit.
2. Replace the local copy verbatim.
3. Update the pinned commit SHA in the table above.
4. Re-generate any code derived from the spec (e.g. Go shapes
   transcribed from `a2a.proto`).
5. Add a glossary entry for any new vocabulary the spec introduces.
6. The PR title MUST start with `deps(specs):` so reviewers can
   filter spec bumps from runtime changes.
