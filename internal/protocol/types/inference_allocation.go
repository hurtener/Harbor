package types

// InferenceAllocation is immutable accepted token and optional monetary funding.
// Hard monetary limits require an exact operator-installed pricing reference.
type InferenceAllocation struct {
	AllocationID            string `json:"allocation_id"`
	Revision                uint64 `json:"revision"`
	MaxTotalTokens          int64  `json:"max_total_tokens"`
	MaxCostMicroUSD         *int64 `json:"max_cost_micro_usd,omitempty"`
	PricingManifestID       string `json:"pricing_manifest_id,omitempty"`
	PricingManifestRevision uint64 `json:"pricing_manifest_revision,omitempty"`
	PricingManifestSHA256   string `json:"pricing_manifest_sha256,omitempty"`
}

// InferenceAllocationSnapshot is content-free durable provider accounting.
// Reserved tokens include crash/transport uncertainty and do not expire.
type InferenceAllocationSnapshot struct {
	// Closed is an irreversible barrier to new provider reservations. Existing
	// reserved/unknown attempts remain liability even after this becomes true.
	Closed          bool   `json:"closed"`
	MaxCostMicroUSD *int64 `json:"max_cost_micro_usd,omitempty"`
	// ChargedCostMicroUSD is consumed conservative ceiling capacity, not actual spend.
	ChargedCostMicroUSD int64 `json:"charged_cost_micro_usd"`
	// ReservedCostMicroUSD includes unresolved provider liability and never expires.
	ReservedCostMicroUSD int64 `json:"reserved_cost_micro_usd"`
	// UnknownCostMicroUSD is the unproven portion of reserved monetary capacity.
	UnknownCostMicroUSD     int64                        `json:"unknown_cost_micro_usd"`
	PricingManifestID       string                       `json:"pricing_manifest_id,omitempty"`
	PricingManifestRevision uint64                       `json:"pricing_manifest_revision,omitempty"`
	PricingManifestSHA256   string                       `json:"pricing_manifest_sha256,omitempty"`
	Receipts                []InferenceAllocationReceipt `json:"receipts"`
	ReceiptsTruncated       bool                         `json:"receipts_truncated"`
	BoundBreached           bool                         `json:"bound_breached"`
	AllocationID            string                       `json:"allocation_id"`
	Revision                uint64                       `json:"revision"`
	MaxTotalTokens          int64                        `json:"max_total_tokens"`
	SettledTokens           int64                        `json:"settled_tokens"`
	ReservedTokens          int64                        `json:"reserved_tokens"`
	UnknownTokens           int64                        `json:"unknown_tokens"`
	AttemptCount            int64                        `json:"attempt_count"`
	Guarantee               string                       `json:"guarantee"`
	PricingStatus           string                       `json:"pricing_status"`
}

// InferenceAllocationReceipt is a content-free terminal provider-envelope accounting receipt.
// Unknown tokens remain held; settled usage never implies unknown work is free.
type InferenceAllocationReceipt struct {
	// ReservedCostMicroUSD includes unresolved provider liability and never expires.
	ReservedCostMicroUSD int64 `json:"reserved_cost_micro_usd"`
	// ChargedCostMicroUSD is consumed conservative ceiling capacity, not actual spend.
	ChargedCostMicroUSD int64 `json:"charged_cost_micro_usd"`
	// UnknownCostMicroUSD is the unproven portion of reserved monetary capacity.
	UnknownCostMicroUSD int64 `json:"unknown_cost_micro_usd"`
	// MonetaryStatus is charged_ceiling, unknown, or released_before_dispatch.
	MonetaryStatus string `json:"monetary_status,omitempty"`
	AttemptID      string `json:"attempt_id"`
	ReservedTokens int64  `json:"reserved_tokens"`
	SettledTokens  int64  `json:"settled_tokens"`
	UnknownTokens  int64  `json:"unknown_tokens"`
	Status         string `json:"status"`
}
