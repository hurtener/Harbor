package serve

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/agentcfg/sessionoverlay"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/skills"
	"github.com/hurtener/Harbor/internal/state"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
)

func newSessionPersonalAuthorityDeps(t *testing.T) (state.StateStore, skills.SkillStore) {
	t.Helper()
	st, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close(context.Background()) })
	store, err := skills.Open(t.Context(), skills.ConfigSnapshot{Driver: "localdb", DSN: ":memory:"}, skills.Deps{Bus: mkDriverTestBus(t, auditpatterns.New())})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })
	return st, store
}

func TestNewSessionPersonalSkillAuthority_ResumesDeclaredCutoverAcrossRestart(t *testing.T) {
	st, store := newSessionPersonalAuthorityDeps(t)
	declarations := []config.SessionPersonalCutoverTenant{
		{TenantID: "drained", Epoch: "epoch-1", RosterDigest: "digest-1", LegacyWritersDrained: true},
		{TenantID: "read-only", Epoch: "epoch-2", RosterDigest: "digest-2", LegacyWritersDrained: false},
	}
	first, err := NewSessionPersonalSkillAuthority(t.Context(), st, store, declarations)
	if err != nil {
		t.Fatal(err)
	}
	if first.Personal == nil || first.Cutover == nil || first.Controller == nil {
		t.Fatalf("incomplete authority = %+v", first)
	}
	if mode, modeErr := first.Cutover.Mode(t.Context(), "drained"); modeErr != nil || mode != sessionoverlay.CutoverStateOnly {
		t.Fatalf("drained mode = (%q, %v), want state_only", mode, modeErr)
	}
	if mode, modeErr := first.Cutover.Mode(t.Context(), "read-only"); modeErr != nil || mode != sessionoverlay.CutoverDualRead {
		t.Fatalf("undrained mode = (%q, %v), want dual_read", mode, modeErr)
	}

	// A fresh graph over the same StateStore consumes the durable checkpoint;
	// it does not depend on process-local migration state.
	restarted, err := NewSessionPersonalSkillAuthority(t.Context(), st, store, declarations)
	if err != nil {
		t.Fatal(err)
	}
	if mode, modeErr := restarted.Cutover.Mode(t.Context(), "drained"); modeErr != nil || mode != sessionoverlay.CutoverStateOnly {
		t.Fatalf("restarted mode = (%q, %v), want state_only", mode, modeErr)
	}
}

type authorityScanLimitStore struct {
	state.StateStore
	limits []int
}

func (s *authorityScanLimitStore) ScanKindForTenant(
	ctx context.Context,
	scope state.ListScope,
	tenantID string,
	literalKindPrefix string,
	limit int,
	continuation string,
) (state.StateScanPage, error) {
	s.limits = append(s.limits, limit)
	return s.StateStore.ScanKindForTenant(ctx, scope, tenantID, literalKindPrefix, limit, continuation)
}

func TestNewSessionPersonalSkillAuthority_BoundsEveryBootScanPageAndHonorsCancellation(t *testing.T) {
	st, store := newSessionPersonalAuthorityDeps(t)
	bounded := &authorityScanLimitStore{StateStore: st}
	declaration := []config.SessionPersonalCutoverTenant{{
		TenantID: "bounded", Epoch: "epoch", RosterDigest: "digest", LegacyWritersDrained: true,
	}}
	if _, err := NewSessionPersonalSkillAuthority(t.Context(), bounded, store, declaration); err != nil {
		t.Fatal(err)
	}
	if len(bounded.limits) < 2 {
		t.Fatalf("boot scans = %v, want copy and fresh verification scans", bounded.limits)
	}
	for _, limit := range bounded.limits {
		if limit != state.MaxStateScanLimit {
			t.Fatalf("boot scan limit = %d, want bounded maximum %d", limit, state.MaxStateScanLimit)
		}
	}

	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := NewSessionPersonalSkillAuthority(canceled, st, store, declaration); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled boot error = %v, want context.Canceled", err)
	}
}

func TestNewSessionPersonalSkillAuthority_FailsLoudOnPartialDependencies(t *testing.T) {
	st, store := newSessionPersonalAuthorityDeps(t)
	for _, tc := range []struct {
		name  string
		state state.StateStore
		skill skills.SkillStore
	}{
		{name: "state", skill: store},
		{name: "skills", state: st},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewSessionPersonalSkillAuthority(t.Context(), tc.state, tc.skill, nil); err == nil || !strings.Contains(err.Error(), "required") {
				t.Fatalf("partial authority error = %v", err)
			}
		})
	}
}

func TestNewRunLoopDriver_SkillsDirectoryRequiresCompleteSnapshotAuthority(t *testing.T) {
	_, store := newSessionPersonalAuthorityDeps(t)
	env := newFailDriverEnv(t)
	directory, err := skills.NewDirectory(store, skills.Deps{Bus: env.bus}, skills.DirectoryConfig{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = NewRunLoopDriver(RunLoopDriverOptions{
		Bus:             env.bus,
		RunLoop:         env.rl,
		Planner:         &driverTestPlanner{finishGoalImmediately: true},
		Tasks:           env.reg,
		SkillsDirectory: directory,
	})
	if !errors.Is(err, ErrRunLoopDriverMisconfigured) || !strings.Contains(err.Error(), "complete run snapshot authority") {
		t.Fatalf("directory-only wiring error = %v, want complete-authority refusal", err)
	}
}
