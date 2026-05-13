---
name: failure-modes-only
trigger: when the skill lists failure modes but no preconditions
---
A skill that enumerates failure modes without preconditions.

## Steps

- Attempt the operation.

## Failure modes

- Operation times out: retry once then surface ErrTimeout.
- Operation conflicts with another tenant: surface ErrConflict.
