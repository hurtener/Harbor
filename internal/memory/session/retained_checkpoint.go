package session

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/planner/trajectory"
)

// A checkpoint represents a committed prefix, including detail already removed
// by compaction. Remaining source detail is digest-bound until it too is covered.
// CompactedThrough lives on the window so invalidation cannot erase the frontier.
type retainedCheckpoint struct {
	Version       int               `json:"version"`
	Generation    uint64            `json:"generation"`
	SourceThrough retainedAdmission `json:"source_through"`
	SourceDigest  string            `json:"source_digest"`
	ThroughStep   int               `json:"through_step"`
	ExpiresAt     time.Time         `json:"expires_at"`
	Narrative     *planner.Summary  `json:"narrative"`
}

func retainedDigest(value any) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", ErrRetainedContextUnavailable
	}
	// Canonicalize nested RawMessages and exact numeric lexemes alike.
	var tree any
	if err := decodeRetained(encoded, &tree); err != nil {
		return "", err
	}
	encoded, err = json.Marshal(tree)
	if err != nil {
		return "", ErrRetainedContextUnavailable
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func checkpointSources(window retainedWindow) ([]retainedTurn, error) {
	c := window.Checkpoint
	if c == nil || c.SourceThrough.ID == "" || c.SourceThrough.RunID == "" {
		return nil, ErrRetainedContextUnavailable
	}
	if c.SourceThrough == window.CompactedThrough {
		return []retainedTurn{}, nil
	}
	for i, turn := range window.Turns {
		if turn.Admission == c.SourceThrough {
			return window.Turns[:i+1], nil
		}
	}
	return nil, ErrRetainedContextUnavailable
}

func validateRetainedCheckpoint(window retainedWindow) error {
	c := window.Checkpoint
	if c == nil {
		return nil
	}
	if c.Version != 2 || c.Generation == 0 || c.Generation != window.Generation || c.ThroughStep < 1 || c.ExpiresAt.IsZero() || c.Narrative == nil || c.Narrative.Coverage != nil || !c.Narrative.HasContent() {
		return ErrRetainedContextUnavailable
	}
	sources, err := checkpointSources(window)
	if err != nil {
		return err
	}
	for _, source := range sources {
		if c.ExpiresAt.After(source.ExpiresAt) {
			return ErrRetainedContextUnavailable
		}
	}
	digest, err := retainedDigest(sources)
	if err != nil || digest != c.SourceDigest {
		return ErrRetainedContextUnavailable
	}
	// Whole turn queries/outcomes frame the exact exchanges. Only validated
	// envelopes are eligible, including entries currently hidden by the summary.
	prefix, err := projectRetainedWindow(retainedWindow{Turns: sources, CompactedThrough: window.CompactedThrough})
	if err != nil || c.ThroughStep > len(prefix) {
		return ErrRetainedContextUnavailable
	}
	return nil
}

func (r *RetainedRun) applyCheckpoint(tr *planner.Trajectory) error {
	if r.checkpoint == nil {
		return nil
	}
	c := r.checkpoint
	summary := trajectory.CloneSummary(c.Narrative)
	digest, err := tr.PrefixDigest(c.ThroughStep)
	if err != nil {
		return err
	}
	summary.Coverage = &planner.SummaryCoverage{Version: 1, Generation: c.Generation, ThroughStep: c.ThroughStep, PrefixDigest: digest}
	tr.Summary = summary
	r.appliedSummary = summary
	return nil
}

// Translate a newly generated run checkpoint to the canonical retained view.
// Live call IDs/action types are wrapped by RetainStep, never re-executed. The
// stored digest binds the entire source set, including the summarizer's current
// query, not merely the exchanges preceding its coverage cursor.
func (r *RetainedRun) retainCheckpoint(ctx context.Context, tr *planner.Trajectory, turn retainedTurn, window retainedWindow) (*retainedCheckpoint, error) {
	if tr.Summary == nil || tr.Summary == r.appliedSummary || tr.Summary.Coverage == nil {
		return nil, nil
	}
	through, err := tr.ReplayStart()
	if err != nil {
		return nil, err
	}
	// A concurrent/unknown run notice may already be stale at publication. Keep
	// the exact history instead of transporting that generated status as fact.
	if r.hadActivePrefix || tr.Query != turn.Query {
		return nil, nil
	}
	// A checkpoint generated from a frozen admission cannot overwrite another
	// sibling's checkpoint or an invalidation. CAS on the window then protects
	// this comparison through the terminal publication transaction.
	if window.Generation != r.generation || window.CompactedThrough != r.compactedThrough {
		return nil, nil
	}
	beforeCheckpoint, err := retainedDigest(r.checkpoint)
	if err != nil {
		return nil, err
	}
	currentCheckpoint, err := retainedDigest(window.Checkpoint)
	if err != nil {
		return nil, err
	}
	if beforeCheckpoint != currentCheckpoint {
		return nil, nil
	}
	sources := append(append([]retainedTurn(nil), r.sourceTurns...), turn)
	if len(sources) > len(window.Turns) {
		return nil, nil
	}
	for i, source := range sources {
		if source.Admission != window.Turns[i].Admission {
			return nil, nil
		}
	}
	generation := max(window.Generation+1, tr.Summary.Coverage.Generation)
	if window.Generation+1 == 0 {
		return nil, ErrRetainedContextCapacity
	}
	c := &retainedCheckpoint{Version: 2, Generation: generation, SourceThrough: turn.Admission, ExpiresAt: turn.ExpiresAt}
	for _, source := range sources {
		if source.ExpiresAt.Before(c.ExpiresAt) {
			c.ExpiresAt = source.ExpiresAt
		}
	}
	if r.checkpoint != nil && r.checkpoint.ExpiresAt.Before(c.ExpiresAt) {
		c.ExpiresAt = r.checkpoint.ExpiresAt
	}
	want, err := retainedDigest(sources)
	if err != nil {
		return nil, err
	}
	actual, err := retainedDigest(window.Turns[:len(sources)])
	if err != nil {
		return nil, err
	}
	if actual != want {
		return nil, nil
	}
	// Covered inherited evidence must still be the frozen admission view.
	if len(tr.Steps) < len(r.prefix) {
		return nil, ErrRetainedContextUnavailable
	}
	if len(r.prefix) > 0 {
		expected, err := projectRetainedWindow(retainedWindow{Turns: r.sourceTurns, Partial: r.hadPartialPrefix, CompactedThrough: r.compactedThrough})
		if err != nil {
			return nil, err
		}
		before, err := retainedDigest(expected)
		if err != nil {
			return nil, err
		}
		after, err := retainedDigest(tr.Steps[:len(r.prefix)])
		if err != nil {
			return nil, err
		}
		if before != after {
			return nil, nil
		}
	}
	stablePrefix, err := projectRetainedWindow(retainedWindow{Turns: r.sourceTurns, CompactedThrough: r.compactedThrough})
	if err != nil {
		return nil, err
	}
	c.ThroughStep = min(through, len(stablePrefix))
	if through >= r.prefixLen {
		ownCovered := through - r.prefixLen
		if ownCovered > len(turn.Steps) {
			return nil, ErrRetainedContextUnavailable
		}
		// Redaction/failure scrubbing can change the source that generated prose.
		// Do not certify such prose as a summary of the persisted evidence.
		for i := range ownCovered {
			var stored planner.Step
			if err := decodeRetained(turn.Steps[i], &stored); err != nil {
				return nil, err
			}
			restored, err := planner.ReadHistoricalStep(stored)
			if err != nil {
				return nil, err
			}
			before, err := retainedDigest(trajectory.ModelStep(tr.Steps[r.prefixLen+i]))
			if err != nil {
				return nil, err
			}
			after, err := retainedDigest(trajectory.ModelStep(restored))
			if err != nil {
				return nil, err
			}
			if before != after {
				return nil, nil
			}
		}
		// The live current-request delimiter becomes the retained turn's user query.
		c.ThroughStep = len(stablePrefix) + 1 + ownCovered
	}
	if c.ThroughStep == 0 {
		return nil, nil
	}
	narrative := trajectory.CloneSummary(tr.Summary)
	narrative.Coverage = nil
	data, err := r.redactJournalValue(ctx, narrative)
	if err != nil {
		return nil, err
	}
	if err := decodeRetained(data, &c.Narrative); err != nil {
		return nil, err
	}
	if c.Narrative == nil || !c.Narrative.HasContent() || c.Narrative.Coverage != nil {
		return nil, ErrRetainedContextUnavailable
	}
	c.SourceDigest = actual
	candidate := window
	candidate.Checkpoint = c
	candidate.Generation = c.Generation
	if err := validateRetainedCheckpoint(candidate); err != nil {
		return nil, err
	}
	return c, nil
}

// Expiry removes information, unlike successful compaction. Losing any remaining
// source invalidates the derived checkpoint rather than certifying selective loss.
func pruneRetainedCheckpoint(window *retainedWindow) {
	if window.Checkpoint != nil {
		if _, err := checkpointSources(*window); err != nil {
			window.Checkpoint = nil
		}
	}
}

// discardCoveredTurn changes representation only after a checkpoint represents
// the complete oldest turn. The caller publishes this and the checkpoint in one
// conditional write; no storage deletion happens before that transaction.
func discardCoveredTurn(window *retainedWindow) error {
	if len(window.Turns) == 0 || window.Checkpoint == nil {
		return ErrRetainedContextCapacity
	}
	oldest := window.Turns[0]
	for _, active := range window.Active {
		if active.Sequence <= oldest.Admission.Sequence {
			return ErrRetainedContextCapacity // never advance past a late sibling
		}
	}
	anchor := 0
	if window.CompactedThrough.ID != "" {
		anchor = 1
	}
	covered := len(oldest.Steps) + 2 // admitted query, whole exchanges, outcome
	if window.Checkpoint.ThroughStep < anchor+covered {
		return ErrRetainedContextCapacity
	}
	// Preserve exact execution evidence separately from the lossy narrative.
	// Retention lifetime and erasure govern this evidence, not an independent
	// count ceiling that could eventually prevent successful compaction.
	if len(oldest.Steps) > 0 {
		window.Evidence = append(window.Evidence, retainedEvidence{Admission: oldest.Admission, ExpiresAt: oldest.ExpiresAt, Steps: oldest.Steps})
	}
	c := *window.Checkpoint
	c.ThroughStep = c.ThroughStep - covered + 1 - anchor
	window.CompactedThrough = oldest.Admission
	window.Turns = window.Turns[1:]
	window.Checkpoint = &c
	sources, err := checkpointSources(*window)
	if err != nil {
		return err
	}
	c.SourceDigest, err = retainedDigest(sources)
	return err
}

// The redactor operates on a detached tree, which may reorder JSON. Compare
// semantic content while retaining exact numbers; formatting is not authority.
func sameRetainedContent(a, b retainedTurn) bool {
	before, err := retainedDigest(a)
	if err != nil {
		return false
	}
	after, err := retainedDigest(b)
	return err == nil && before == after
}
