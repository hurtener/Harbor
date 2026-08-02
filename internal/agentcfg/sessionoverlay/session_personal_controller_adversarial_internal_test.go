package sessionoverlay

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/skills"
	"github.com/hurtener/Harbor/internal/state"
)

type cutoverReadResult struct {
	mode CutoverMode
	err  error
}

type sequenceCutoverReader struct{ results []cutoverReadResult }

func (r *sequenceCutoverReader) Mode(context.Context, string) (CutoverMode, error) {
	if len(r.results) == 0 {
		return CutoverDualRead, errors.New("unexpected cutover read")
	}
	result := r.results[0]
	r.results = r.results[1:]
	return result.mode, result.err
}

func TestSessionPersonalController_PublicValidationAndDependencyErrors(t *testing.T) {
	id := identity.Quadruple{Identity: identity.Identity{TenantID: "tenant", UserID: "user", SessionID: "session"}, RunID: "run"}
	var nilController *SessionPersonalController
	if _, err := nilController.SessionSkills(context.Background(), id, "agent-a"); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil controller=%v", err)
	}
	controller := &SessionPersonalController{
		personal: &DurableStore{state: resolverBoundaryStateStore{}, clock: time.Now},
		cutover:  fixedCutoverReader{mode: CutoverStateOnly}, legacy: boundarySkillReader{},
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := controller.SessionSkills(cancelled, id, "agent-a"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled read=%v", err)
	}
	if err := controller.UpsertSessionSkill(cancelled, id, "agent-a", trustBoundarySkill("x")); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled upsert=%v", err)
	}
	if err := controller.DeleteSessionSkill(cancelled, id, "agent-a", "x"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled delete=%v", err)
	}
	invalid := trustBoundarySkill("invalid")
	invalid.Trigger = ""
	if err := controller.UpsertSessionSkill(context.Background(), id, "agent-a", invalid); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("invalid body=%v", err)
	}
	wrongScope := trustBoundarySkill("wrong-scope")
	wrongScope.Scope = skills.ScopeUser
	if err := controller.UpsertSessionSkill(context.Background(), id, "agent-a", wrongScope); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("wrong scope=%v", err)
	}
	if err := controller.DeleteSessionSkill(context.Background(), id, "agent-a", " "); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("empty delete=%v", err)
	}

	dependencyErr := errors.New("mode offline")
	controller.cutover = fixedCutoverReader{err: dependencyErr}
	if _, err := controller.loadCutoverMode(context.Background(), id.TenantID); !errors.Is(err, dependencyErr) {
		t.Fatalf("mode error=%v", err)
	}
	controller.cutover = fixedCutoverReader{mode: "future"}
	if err := controller.requireStateOnly(context.Background(), id, "agent-a"); !errors.Is(err, ErrAgentLifecycleInactive) {
		// Fences are checked before mode, so missing lifecycle must remain the
		// authority error and the malformed mode cannot mask it.
		t.Fatalf("fence-before-mode error=%v", err)
	}
}

func TestSessionPersonalController_LegacyTierExactIdentityAliasAndErrors(t *testing.T) {
	id := identity.Quadruple{Identity: identity.Identity{TenantID: "tenant", UserID: "user", SessionID: "session"}, RunID: "run"}
	q := durableSessionQuad(id)
	kind := LegacyOverlayKind("agent-a")
	dependencyErr := errors.New("legacy storage offline")
	controller := &SessionPersonalController{
		personal: &DurableStore{state: resolverBoundaryStateStore{load: func(context.Context, identity.Quadruple, string) (state.StateRecord, error) {
			return state.StateRecord{}, dependencyErr
		}}},
		cutover: fixedCutoverReader{mode: CutoverDualRead}, legacy: boundarySkillReader{},
	}
	if _, err := controller.loadLegacyTier(context.Background(), id, "agent-a"); !errors.Is(err, ErrStateUnavailable) {
		t.Fatalf("legacy load error=%v", err)
	}

	wrongIdentity := state.StateRecord{Identity: identity.Quadruple{Identity: identity.Identity{TenantID: "tenant", UserID: "other", SessionID: "session"}}, Kind: kind, Bytes: []byte(`{"schema":1,"overlay":{},"updated_at":"2026-08-02T00:00:00Z"}`)}
	controller.personal.state = resolverBoundaryStateStore{load: func(context.Context, identity.Quadruple, string) (state.StateRecord, error) {
		return wrongIdentity, nil
	}}
	if _, err := controller.loadLegacyTier(context.Background(), id, "agent-a"); !errors.Is(err, ErrLegacyOverlayInvalid) {
		t.Fatalf("legacy identity mismatch=%v", err)
	}

	alpha := trustBoundarySkill("alpha")
	other := alpha
	other.Description = "different alias body"
	other.ContentHash = skills.CanonicalContentHash(other)
	record := state.StateRecord{Identity: q, Kind: kind, Bytes: []byte(`{"schema":1,"overlay":{"personal_skills":["ALPHA","alpha"]},"updated_at":"2026-08-02T00:00:00Z"}`)}
	controller.personal.state = resolverBoundaryStateStore{load: func(context.Context, identity.Quadruple, string) (state.StateRecord, error) { return record, nil }}
	controller.legacy = boundarySkillReader{byName: map[string]skills.Skill{"ALPHA": alpha, "alpha": other}}
	if _, err := controller.loadLegacyTier(context.Background(), id, "agent-a"); !errors.Is(err, ErrLegacySkillInvalid) {
		t.Fatalf("legacy alias disagreement=%v", err)
	}

	controller.legacy = boundarySkillReader{err: dependencyErr}
	single := record
	single.Bytes = []byte(`{"schema":1,"overlay":{"personal_skills":["alpha"]},"updated_at":"2026-08-02T00:00:00Z"}`)
	controller.personal.state = resolverBoundaryStateStore{load: func(context.Context, identity.Quadruple, string) (state.StateRecord, error) { return single, nil }}
	if _, err := controller.loadLegacyTier(context.Background(), id, "agent-a"); !errors.Is(err, ErrLegacySkillInvalid) {
		t.Fatalf("legacy reader error=%v", err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	controller.legacy = boundarySkillReader{byName: map[string]skills.Skill{"alpha": alpha}}
	if _, err := controller.loadLegacyTier(cancelled, id, "agent-a"); !errors.Is(err, context.Canceled) {
		t.Fatalf("legacy enumeration cancellation=%v", err)
	}
}

func TestSessionPersonalController_OwnedTierBoundsIdentityDuplicatesAndDecode(t *testing.T) {
	id := identity.Quadruple{Identity: identity.Identity{TenantID: "tenant", UserID: "user", SessionID: "session"}, RunID: "run"}
	q := durableSessionQuad(id)
	dependencyErr := errors.New("owned storage offline")
	controller := &SessionPersonalController{
		personal: &DurableStore{state: resolverBoundaryStateStore{listBounded: func(context.Context, identity.Quadruple, string, int) ([]state.StateRecord, error) {
			return nil, dependencyErr
		}}},
		cutover: fixedCutoverReader{mode: CutoverStateOnly}, legacy: boundarySkillReader{},
	}
	if _, err := controller.loadOwnedTier(context.Background(), id, "agent-a"); !errors.Is(err, ErrStateUnavailable) {
		t.Fatalf("owned list error=%v", err)
	}

	skill := trustBoundarySkill("alpha")
	personal := PersonalSkillRecord{Schema: 1, AgentID: "agent-a", CanonicalName: "alpha", ContentHash: skill.ContentHash, Skill: skill, UpdatedAt: time.Now().UTC()}
	kind, err := PersonalSkillKind("agent-a", "alpha")
	if err != nil {
		t.Fatal(err)
	}
	record := state.StateRecord{Identity: q, Kind: kind, Bytes: trustBoundaryJSON(t, personal)}
	wrongIdentity := record
	wrongIdentity.Identity.UserID = "other"
	controller.personal.state = resolverBoundaryStateStore{listBounded: func(context.Context, identity.Quadruple, string, int) ([]state.StateRecord, error) {
		return []state.StateRecord{wrongIdentity}, nil
	}}
	if _, err := controller.loadOwnedTier(context.Background(), id, "agent-a"); !errors.Is(err, ErrPersonalRecordInvalid) {
		t.Fatalf("owned identity mismatch=%v", err)
	}
	controller.personal.state = resolverBoundaryStateStore{listBounded: func(context.Context, identity.Quadruple, string, int) ([]state.StateRecord, error) {
		return []state.StateRecord{record, record}, nil
	}}
	if _, err := controller.loadOwnedTier(context.Background(), id, "agent-a"); !errors.Is(err, ErrPersonalRecordInvalid) {
		t.Fatalf("owned duplicate canonical name=%v", err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	controller.personal.state = resolverBoundaryStateStore{listBounded: func(context.Context, identity.Quadruple, string, int) ([]state.StateRecord, error) {
		return []state.StateRecord{record}, nil
	}}
	if _, err := controller.loadOwnedTier(cancelled, id, "agent-a"); !errors.Is(err, context.Canceled) {
		t.Fatalf("owned enumeration cancellation=%v", err)
	}

	for _, tc := range []struct {
		name   string
		record state.StateRecord
	}{
		{name: "duplicate", record: state.StateRecord{Kind: kind, Bytes: []byte(`{"canonical_name":"alpha","canonical_name":"alpha"}`)}},
		{name: "malformed", record: state.StateRecord{Kind: kind, Bytes: []byte(`{`)}},
		{name: "canonical absent", record: state.StateRecord{Kind: kind, Bytes: []byte(`{}`)}},
		{name: "invalid payload", record: state.StateRecord{Kind: kind, Bytes: []byte(`{"canonical_name":"alpha"}`)}},
		{name: "wrong key", record: state.StateRecord{Kind: kind + "x", Bytes: record.Bytes}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := decodeControllerPersonal(tc.record, "agent-a"); !errors.Is(err, ErrPersonalRecordInvalid) {
				t.Fatalf("decode error=%v", err)
			}
		})
	}
	badHash := skill
	badHash.ContentHash = strings.Repeat("0", 64)
	if err := validateControllerLegacySkill(badHash, "alpha"); !errors.Is(err, ErrLegacySkillInvalid) {
		t.Fatalf("legacy noncanonical hash=%v", err)
	}
}

func TestSessionPersonalController_ReadPropagatesBeforeAfterFenceAndModeFailures(t *testing.T) {
	id := identity.Quadruple{Identity: identity.Identity{TenantID: "tenant", UserID: "user", SessionID: "session"}}
	active := []boundaryLoadResult{{record: activeLifecycleBoundaryRecord()}, noRecordBoundaryLoad(), noRecordBoundaryLoad()}
	legacyAbsent := noRecordBoundaryLoad()
	beforeFenceErr := errors.New("before offline")
	afterFenceErr := errors.New("after offline")
	afterModeErr := errors.New("mode reread offline")
	for _, tc := range []struct {
		name    string
		loads   []boundaryLoadResult
		cutover CutoverModeReader
		want    error
	}{
		{name: "before fence storage failure", loads: []boundaryLoadResult{{err: beforeFenceErr}}, cutover: fixedCutoverReader{mode: CutoverDualRead}, want: ErrStateUnavailable},
		{name: "after fence storage failure", loads: append(append([]boundaryLoadResult{}, active...), legacyAbsent, boundaryLoadResult{err: afterFenceErr}), cutover: fixedCutoverReader{mode: CutoverDualRead}, want: ErrStateUnavailable},
		{name: "after retirement", loads: append(append([]boundaryLoadResult{}, active...), legacyAbsent, boundaryLoadResult{record: terminalLifecycleBoundaryRecord()}, noRecordBoundaryLoad(), noRecordBoundaryLoad()), cutover: fixedCutoverReader{mode: CutoverDualRead}, want: agentcfg.ErrAgentRetired},
		{name: "after mode failure", loads: append(append(append([]boundaryLoadResult{}, active...), legacyAbsent), active...), cutover: &sequenceCutoverReader{results: []cutoverReadResult{{mode: CutoverDualRead}, {err: afterModeErr}}}, want: afterModeErr},
	} {
		t.Run(tc.name, func(t *testing.T) {
			controller := &SessionPersonalController{
				personal: &DurableStore{state: &scriptedBoundaryStateStore{loads: append([]boundaryLoadResult(nil), tc.loads...)}, clock: time.Now},
				cutover:  tc.cutover, legacy: boundarySkillReader{},
			}
			if _, err := controller.SessionSkills(context.Background(), id, "agent-a"); !errors.Is(err, tc.want) {
				t.Fatalf("error=%v want errors.Is(%v)", err, tc.want)
			}
		})
	}
	mutationController := &SessionPersonalController{
		personal: &DurableStore{state: &scriptedBoundaryStateStore{loads: []boundaryLoadResult{{err: errors.New("fence offline")}}}, clock: time.Now},
		cutover:  fixedCutoverReader{mode: CutoverStateOnly}, legacy: boundarySkillReader{},
	}
	if err := mutationController.DeleteSessionSkill(context.Background(), id, "agent-a", "alpha"); !errors.Is(err, ErrStateUnavailable) {
		t.Fatalf("mutation fence load error=%v", err)
	}
}
