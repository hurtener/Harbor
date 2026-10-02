package types

// InferenceAllocation is immutable accepted task funding in integer tokens.
// Hard monetary limits are rejected until trusted pricing is available.
type InferenceAllocation struct {
	AllocationID    string `json:"allocation_id"`
	Revision        uint64 `json:"revision"`
	MaxTotalTokens  int64  `json:"max_total_tokens"`
	MaxCostMicroUSD *int64 `json:"max_cost_micro_usd,omitempty"`
}

// InferenceAllocationSnapshot is content-free durable provider accounting.
// Reserved tokens include crash/transport uncertainty and do not expire.
type InferenceAllocationSnapshot struct {
	Receipts          []InferenceAllocationReceipt `json:"receipts"`
	ReceiptsTruncated bool                         `json:"receipts_truncated"`
	BoundBreached     bool                         `json:"bound_breached"`
	AllocationID      string                       `json:"allocation_id"`
	Revision          uint64                       `json:"revision"`
	MaxTotalTokens    int64                        `json:"max_total_tokens"`
	SettledTokens     int64                        `json:"settled_tokens"`
	ReservedTokens    int64                        `json:"reserved_tokens"`
	UnknownTokens     int64                        `json:"unknown_tokens"`
	AttemptCount      int64                        `json:"attempt_count"`
	Guarantee         string                       `json:"guarantee"`
	PricingStatus     string                       `json:"pricing_status"`
}

// InferenceAllocationReceipt is a content-free terminal provider-envelope accounting receipt.
// Unknown tokens remain held; settled usage never implies unknown work is free.
type InferenceAllocationReceipt struct {
	AttemptID      string `json:"attempt_id"`
	ReservedTokens int64  `json:"reserved_tokens"`
	SettledTokens  int64  `json:"settled_tokens"`
	UnknownTokens  int64  `json:"unknown_tokens"`
	Status         string `json:"status"`
}
