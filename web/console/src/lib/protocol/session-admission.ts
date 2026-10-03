// Canonical durable session admission wire types.
import type { SessionIdentityScope } from '../sessions/types.js';

/** The authenticated admin's compare-and-set admission epoch transition. */
export interface SessionsSetAdmissionRequest {
  identity: SessionIdentityScope;
  expected_epoch: number;
  epoch: number;
}

/** The installed epoch; earlier accepted work may still be running. */
export interface SessionsSetAdmissionResponse {
  epoch: number;
  protocol_version: string;
}
