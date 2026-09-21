// Canonical retained-context reconciliation wire types. The operation is an
// explicit own-session mutation, not a resume or permission to retry a write.
import type { SessionIdentityScope } from '../sessions/types.js';

/** Mirrors internal/protocol/types.SessionsReconcileContextRequest. */
export interface SessionsReconcileContextRequest {
  identity: SessionIdentityScope;
  source_run_id: string;
}

/** A durable-context acknowledgement, not proof of successful execution. */
export interface SessionsReconcileContextResponse {
  session_id: string;
  source_run_id: string;
  reconciled: boolean;
}
