# Harbor — RFCs

Merged RFCs land here. The active RFC sits at the repo root (`/RFC-001-Harbor.md`) until merged, then moves here with a frozen timestamp.

## Index

| RFC | Title | Status |
|-----|-------|--------|
| 001 | Harbor — Architecture & V1 Scope | Drafting |

## Authoring an RFC

- File the RFC under `/RFC-NNN-<short-title>.md` for the duration of its drafting.
- The RFC must reference the relevant `docs/research/*.md` briefs for source material.
- Open questions in the RFC become tracked issues before merge.
- After merge, move the file to `docs/rfc/` and update this index.

## Conventions

- An RFC is the highest-priority artifact in the repo (see `/AGENTS.md` §2). Phase plans, code comments, and contributor docs all defer to the RFC.
- An RFC change requires its own PR.
- Breaking changes to a merged RFC are landed as a new RFC that supersedes the old one (`RFC-NNN supersedes RFC-MMM`).
