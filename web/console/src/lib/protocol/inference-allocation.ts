/** Immutable task funding; money caps require an exact trusted pricing reference. */
export interface InferenceAllocation {
  allocation_id: string;
  revision: number;
  max_total_tokens: number;
  max_cost_micro_usd?: number;
  pricing_manifest_id?: string;
  pricing_manifest_revision?: number;
  pricing_manifest_sha256?: string;
}
/** A content-free terminal provider-attempt envelope receipt. */
export interface InferenceAllocationReceipt {
  reserved_cost_micro_usd: number;
  charged_cost_micro_usd: number;
  unknown_cost_micro_usd: number;
  monetary_status?: string;
  attempt_id: string;
  reserved_tokens: number;
  settled_tokens: number;
  unknown_tokens: number;
  status: string;
}
/** Reserved/unknown tokens remain held; elapsed time is never a refund. */
export interface InferenceAllocationSnapshot {
  /** Irreversible reservation barrier; unknown/in-flight liability remains held. */
  closed: boolean;
  max_cost_micro_usd?: number;
  charged_cost_micro_usd: number;
  reserved_cost_micro_usd: number;
  unknown_cost_micro_usd: number;
  pricing_manifest_id?: string;
  pricing_manifest_revision?: number;
  pricing_manifest_sha256?: string;
  receipts: InferenceAllocationReceipt[];
  receipts_truncated: boolean;
  bound_breached: boolean;
  allocation_id: string;
  revision: number;
  max_total_tokens: number;
  settled_tokens: number;
  reserved_tokens: number;
  unknown_tokens: number;
  attempt_count: number;
  guarantee: string;
  pricing_status: string;
}
