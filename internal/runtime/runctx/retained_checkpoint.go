package runctx

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/planner/trajectory"
)

// A retained checkpoint is an optimization of a known, still-retained terminal
// prefix. It never replaces its evidence or extends any source's lifetime.
// One checkpoint lives in the existing bounded session slot; no second log.
type retainedCheckpoint struct {
	Version      int                 `json:"version"`
	Generation   uint64              `json:"generation"`
	Sources      []retainedAdmission `json:"sources"`
	SourceDigest string              `json:"source_digest"`
	ThroughStep  int                 `json:"through_step"`
	Narrative    *planner.Summary    `json:"narrative"`
}

const maxRetainedNarrativeBytes = 16 * 1024

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

func retainedCheckpointMatches(checkpoint *retainedCheckpoint, turns []retainedTurn) bool {
	if checkpoint == nil || len(checkpoint.Sources) == 0 || len(checkpoint.Sources) > len(turns) {
		return false
	}
	for i, source := range checkpoint.Sources {
		if source != turns[i].Admission {
			return false
		}
	}
	return true
}

func validateRetainedCheckpoint(window retainedWindow) error {
	c := window.Checkpoint
	if c == nil {
		return nil
	}
	if c.Version != 1 || c.Generation == 0 || c.ThroughStep < 1 || c.Narrative == nil || c.Narrative.Coverage != nil || !c.Narrative.HasContent() || !retainedCheckpointMatches(c, window.Turns) {
		return ErrRetainedContextUnavailable
	}
	narrative, err := json.Marshal(c.Narrative)
	if err != nil || len(narrative) > maxRetainedNarrativeBytes {
		return ErrRetainedContextUnavailable
	}
	sources := window.Turns[:len(c.Sources)]
	digest, err := retainedDigest(sources)
	if err != nil || digest != c.SourceDigest {
		return ErrRetainedContextUnavailable
	}
	// Whole turn queries/outcomes frame the exact exchanges. Only validated
	// envelopes are eligible, including entries currently hidden by the summary.
	prefix, err := projectRetainedWindow(retainedWindow{Turns: sources})
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
	sources := append(append([]retainedTurn(nil), r.sourceTurns...), turn)
	ids := make([]retainedAdmission, len(sources))
	for i, source := range sources {
		ids[i] = source.Admission
	}
	c := &retainedCheckpoint{Version: 1, Generation: tr.Summary.Coverage.Generation, Sources: ids}
	if !retainedCheckpointMatches(c, window.Turns) {
		return nil, nil
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
		expected, err := projectRetainedWindow(retainedWindow{Turns: r.sourceTurns, Partial: r.hadPartialPrefix})
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
	stablePrefix, err := projectRetainedWindow(retainedWindow{Turns: r.sourceTurns})
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
	if len(data) > maxRetainedNarrativeBytes {
		return nil, ErrRetainedContextCapacity
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
	if err := validateRetainedCheckpoint(candidate); err != nil {
		return nil, err
	}
	return c, nil
}

// Ensure any change to the source turns invalidates rather than resurrects the
// checkpoint. This runs on expected expiry/count/byte eviction, not corruption.
func pruneRetainedCheckpoint(window *retainedWindow) {
	if window.Checkpoint != nil && !retainedCheckpointMatches(window.Checkpoint, window.Turns) {
		window.Checkpoint = nil
	}
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
