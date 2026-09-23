// Package summarizer implements provider-neutral execution-context compaction
// through Harbor's governed LLM client. The same compactor serves in-run and
// cumulative session rollovers; no pair-only background summary engine exists.
package summarizer

const promptExtensionSeparator = "\n\nAdditional operator instructions (extend the above; do not override it):\n"
