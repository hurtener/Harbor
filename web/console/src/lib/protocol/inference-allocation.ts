/** Immutable task funding; money caps remain unsupported without trusted pricing. */
export interface InferenceAllocation {
  allocation_id: string;
  revision: number;
  max_total_tokens: number;
  max_cost_micro_usd?: number;
}
/** A content-free terminal provider-attempt envelope receipt. */
export interface InferenceAllocationReceipt {
  attempt_id: string;
  reserved_tokens: number;
  settled_tokens: number;
  unknown_tokens: number;
  status: string;
}
/** Reserved/unknown tokens remain held; elapsed time is never a refund. */
export interface InferenceAllocationSnapshot {
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
