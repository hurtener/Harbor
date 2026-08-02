package sessionoverlay_test

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/agentcfg/sessionfence"
	"github.com/hurtener/Harbor/internal/agentcfg/sessionoverlay"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/skills"
	"github.com/hurtener/Harbor/internal/state"
	"github.com/hurtener/Harbor/internal/state/drivers/postgres"
	"github.com/hurtener/Harbor/internal/state/drivers/sqlite"
)

const realDriverRaceTimeout = 15 * time.Second

type realDriverOpener func() (state.StateStore, error)

type realDriverTargetBarrier struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func newRealDriverTargetBarrier() *realDriverTargetBarrier {
	return &realDriverTargetBarrier{entered: make(chan struct{}, 2), release: make(chan struct{})}
}

func (b *realDriverTargetBarrier) wait(t *testing.T, count int) {
	t.Helper()
	for range count {
		select {
		case <-b.entered:
		case <-time.After(realDriverRaceTimeout):
			t.Fatal("timed out waiting for independent stores to reach targeted SaveIf")
		}
	}
}

func (b *realDriverTargetBarrier) releaseAll() {
	b.once.Do(func() { close(b.release) })
}

type realDriverBarrierStore struct {
	state.StateStore
	targetKind string
	barrier    *realDriverTargetBarrier
	blocked    atomic.Bool
}

func (s *realDriverBarrierStore) SaveIf(ctx context.Context, expectations []state.SlotExpectation, next state.StateRecord) error {
	if next.Kind == s.targetKind && s.blocked.CompareAndSwap(false, true) {
		s.barrier.entered <- struct{}{}
		select {
		case <-s.barrier.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return s.StateStore.SaveIf(ctx, expectations, next)
}

type realDriverLegacyReader struct{}

func (realDriverLegacyReader) Get(context.Context, identity.Quadruple, string) (skills.Skill, error) {
	return skills.Skill{}, errors.New("real-driver test: legacy Get must not be called in state-only mode")
}

func (realDriverLegacyReader) GetScope(context.Context, identity.Quadruple, string, skills.Scope) (skills.Skill, error) {
	return skills.Skill{}, errors.New("real-driver test: legacy GetScope must not be called in state-only mode")
}

func (realDriverLegacyReader) List(context.Context, identity.Quadruple, skills.ListFilter) ([]skills.Skill, error) {
	return nil, errors.New("real-driver test: legacy List must not be called in state-only mode")
}

func (realDriverLegacyReader) Search(context.Context, identity.Quadruple, string, int) ([]skills.RankedSkill, error) {
	return nil, errors.New("real-driver test: legacy Search must not be called in state-only mode")
}

type realDriverEmptyMigrator struct{}

func (realDriverEmptyMigrator) CopyLegacyOverlay(context.Context, state.StateRecord, config.SessionPersonalCutoverTenant) (int, error) {
	return 0, nil
}

func (realDriverEmptyMigrator) VerifyLegacyOverlay(context.Context, state.StateRecord, config.SessionPersonalCutoverTenant) (bool, error) {
	return true, nil
}

func TestRealDrivers_TwoIndependentSessionPersonalRaceContract(t *testing.T) {
	t.Run("sqlite", func(t *testing.T) {
		dsn := filepath.Join(t.TempDir(), "phase-233a-two-instance.sqlite")
		runRealDriverSessionPersonalRaceContract(t, func() (state.StateStore, error) {
			return sqlite.New(config.StateConfig{Driver: "sqlite", DSN: dsn})
		})
	})

	t.Run("postgres", func(t *testing.T) {
		dsn := realDriverPostgresDSN(t)
		runRealDriverSessionPersonalRaceContract(t, func() (state.StateStore, error) {
			return postgres.New(config.StateConfig{Driver: "postgres", DSN: dsn})
		})
	})
}

func runRealDriverSessionPersonalRaceContract(t *testing.T, opener realDriverOpener) {
	t.Helper()
	ctx := t.Context()
	suffix := realDriverRandomSuffix(t)
	tenantID := "tenant-" + suffix
	agentID := "agent-" + suffix
	id := identity.Quadruple{Identity: identity.Identity{TenantID: tenantID, UserID: "user-" + suffix, SessionID: "session-" + suffix}, RunID: "run-left"}
	rightID := id
	rightID.RunID = "run-right"
	neighborID := identity.Quadruple{Identity: identity.Identity{TenantID: tenantID, UserID: "neighbor-" + suffix, SessionID: id.SessionID}, RunID: "run-neighbor"}
	declaration := config.SessionPersonalCutoverTenant{
		TenantID: tenantID, Epoch: "epoch-" + suffix, RosterDigest: "roster-" + suffix, LegacyWritersDrained: true,
	}

	left := openRealDriverStore(t, opener)
	right := openRealDriverStore(t, opener)
	saveRealDriverLifecycle(t, left, tenantID, agentID, false)

	seedCutover := newRealDriverCutover(t, left, declaration)
	if err := seedCutover.Ensure(ctx); err != nil {
		t.Fatalf("seed dual-read cutover: %v", err)
	}
	preCutover := newRealDriverController(t, left, declaration)
	if err := preCutover.UpsertSessionSkill(ctx, id, agentID, realDriverSkill("before-cutover", "refused")); !errors.Is(err, sessionoverlay.ErrCutoverPending) {
		t.Fatalf("pre-cutover upsert = %v, want ErrCutoverPending", err)
	}

	cutoverKind, err := sessionoverlay.CutoverKind(declaration.Epoch)
	if err != nil {
		t.Fatal(err)
	}
	cutoverBarrier := newRealDriverTargetBarrier()
	cutoverLeft := newRealDriverCutover(t, &realDriverBarrierStore{StateStore: left, targetKind: cutoverKind, barrier: cutoverBarrier}, declaration)
	cutoverRight := newRealDriverCutover(t, &realDriverBarrierStore{StateStore: right, targetKind: cutoverKind, barrier: cutoverBarrier}, declaration)
	cutoverResults := runRealDriverPair(t, cutoverBarrier,
		func() error {
			mode, advanceErr := cutoverLeft.Advance(context.Background(), tenantID, 8, realDriverEmptyMigrator{})
			if advanceErr == nil && mode != sessionoverlay.CutoverStateOnly {
				return fmt.Errorf("left Advance mode = %q, want state_only", mode)
			}
			return advanceErr
		},
		func() error {
			mode, advanceErr := cutoverRight.Advance(context.Background(), tenantID, 8, realDriverEmptyMigrator{})
			if advanceErr == nil && mode != sessionoverlay.CutoverStateOnly {
				return fmt.Errorf("right Advance mode = %q, want state_only", mode)
			}
			return advanceErr
		},
	)
	assertRealDriverOneWinner(t, "cutover checkpoint", cutoverResults)

	closeRealDriverStore(t, left)
	closeRealDriverStore(t, right)
	left = openRealDriverStore(t, opener)
	right = openRealDriverStore(t, opener)
	if mode, modeErr := newRealDriverCutover(t, left, declaration).Mode(ctx, tenantID); modeErr != nil || mode != sessionoverlay.CutoverStateOnly {
		t.Fatalf("restarted cutover mode = (%q, %v), want state_only", mode, modeErr)
	}

	leftController := newRealDriverController(t, left, declaration)
	if err := leftController.UpsertSessionSkill(ctx, neighborID, agentID, realDriverSkill("neighbor-only", "neighbor")); err != nil {
		t.Fatalf("seed neighbor skill: %v", err)
	}

	personalKind, err := sessionoverlay.PersonalSkillKind(agentID, "same-name")
	if err != nil {
		t.Fatal(err)
	}
	upsertBarrier := newRealDriverTargetBarrier()
	upsertLeft := newRealDriverController(t, &realDriverBarrierStore{StateStore: left, targetKind: personalKind, barrier: upsertBarrier}, declaration)
	upsertRight := newRealDriverController(t, &realDriverBarrierStore{StateStore: right, targetKind: personalKind, barrier: upsertBarrier}, declaration)
	upsertResults := runRealDriverPair(t, upsertBarrier,
		func() error {
			return upsertLeft.UpsertSessionSkill(context.Background(), id, agentID, realDriverSkill("same-name", "left-winner"))
		},
		func() error {
			return upsertRight.UpsertSessionSkill(context.Background(), rightID, agentID, realDriverSkill("same-name", "right-winner"))
		},
	)
	firstWinner := assertRealDriverOneWinner(t, "same-slot upsert", upsertResults)
	firstDescription := "left-winner"
	if firstWinner == 1 {
		firstDescription = "right-winner"
	}

	closeRealDriverStore(t, left)
	closeRealDriverStore(t, right)
	left = openRealDriverStore(t, opener)
	right = openRealDriverStore(t, opener)
	assertRealDriverSkills(t, newRealDriverController(t, left, declaration), id, agentID, map[string]string{"same-name": firstDescription})
	assertRealDriverSkills(t, newRealDriverController(t, right, declaration), neighborID, agentID, map[string]string{"neighbor-only": "neighbor"})

	updateDeleteBarrier := newRealDriverTargetBarrier()
	updateController := newRealDriverController(t, &realDriverBarrierStore{StateStore: left, targetKind: personalKind, barrier: updateDeleteBarrier}, declaration)
	deleteController := newRealDriverController(t, &realDriverBarrierStore{StateStore: right, targetKind: personalKind, barrier: updateDeleteBarrier}, declaration)
	updateDeleteResults := runRealDriverPair(t, updateDeleteBarrier,
		func() error {
			return updateController.UpsertSessionSkill(context.Background(), id, agentID, realDriverSkill("same-name", "updated"))
		},
		func() error {
			return deleteController.DeleteSessionSkill(context.Background(), rightID, agentID, "same-name")
		},
	)
	updateWinner := assertRealDriverOneWinner(t, "update/delete", updateDeleteResults) == 0

	closeRealDriverStore(t, left)
	closeRealDriverStore(t, right)
	left = openRealDriverStore(t, opener)
	right = openRealDriverStore(t, opener)
	restartedController := newRealDriverController(t, left, declaration)
	if updateWinner {
		assertRealDriverSkills(t, restartedController, id, agentID, map[string]string{"same-name": "updated"})
	} else {
		assertRealDriverSkills(t, restartedController, id, agentID, map[string]string{})
	}
	assertRealDriverSkills(t, newRealDriverController(t, right, declaration), neighborID, agentID, map[string]string{"neighbor-only": "neighbor"})

	erasureID := identity.Quadruple{Identity: identity.Identity{TenantID: tenantID, UserID: "erased-user-" + suffix, SessionID: "erased-session-" + suffix}}
	erasureKind, err := sessionoverlay.PersonalSkillKind(agentID, "erasure-race")
	if err != nil {
		t.Fatal(err)
	}
	erasureBarrier := newRealDriverTargetBarrier()
	erasureController := newRealDriverController(t, &realDriverBarrierStore{StateStore: left, targetKind: erasureKind, barrier: erasureBarrier}, declaration)
	erasureErr := runRealDriverBlockedMutation(t, erasureBarrier,
		func() error {
			return erasureController.UpsertSessionSkill(context.Background(), erasureID, agentID, realDriverSkill("erasure-race", "must-not-land"))
		},
		func() error {
			q, kind, slotErr := sessionfence.PendingSlot(erasureID)
			if slotErr != nil {
				return slotErr
			}
			return right.Save(context.Background(), state.StateRecord{ID: state.NewEventID(), Identity: q, Kind: kind, Bytes: []byte(`{"pending":true}`)})
		},
	)
	if !errors.Is(erasureErr, state.ErrConditionFailed) {
		t.Fatalf("personal upsert racing erasure = %v, want ErrConditionFailed", erasureErr)
	}
	assertRealDriverTargetAbsent(t, right, erasureID, erasureKind)
	erasureRestart := openRealDriverStore(t, opener)
	erasureDurable := newRealDriverDurable(t, erasureRestart)
	if _, _, loadErr := erasureDurable.LoadPersonal(ctx, erasureID, agentID, "erasure-race"); !errors.Is(loadErr, sessionoverlay.ErrSessionErased) {
		t.Fatalf("restarted erased-session load = %v, want ErrSessionErased", loadErr)
	}

	retiredAgentID := "retired-agent-" + suffix
	retirementID := identity.Quadruple{Identity: identity.Identity{TenantID: tenantID, UserID: "retired-user-" + suffix, SessionID: "retired-session-" + suffix}}
	saveRealDriverLifecycle(t, right, tenantID, retiredAgentID, false)
	retirementKind, err := sessionoverlay.PersonalSkillKind(retiredAgentID, "retirement-race")
	if err != nil {
		t.Fatal(err)
	}
	retirementBarrier := newRealDriverTargetBarrier()
	retirementController := newRealDriverController(t, &realDriverBarrierStore{StateStore: left, targetKind: retirementKind, barrier: retirementBarrier}, declaration)
	retirementErr := runRealDriverBlockedMutation(t, retirementBarrier,
		func() error {
			return retirementController.UpsertSessionSkill(context.Background(), retirementID, retiredAgentID, realDriverSkill("retirement-race", "must-not-land"))
		},
		func() error {
			return saveRealDriverLifecycleError(right, tenantID, retiredAgentID, true)
		},
	)
	if !errors.Is(retirementErr, state.ErrConditionFailed) {
		t.Fatalf("personal upsert racing retirement = %v, want ErrConditionFailed", retirementErr)
	}
	assertRealDriverTargetAbsent(t, right, retirementID, retirementKind)
	retirementRestart := openRealDriverStore(t, opener)
	retirementDurable := newRealDriverDurable(t, retirementRestart)
	if _, _, loadErr := retirementDurable.LoadPersonal(ctx, retirementID, retiredAgentID, "retirement-race"); !errors.Is(loadErr, agentcfg.ErrAgentRetired) {
		t.Fatalf("restarted retired-agent load = %v, want ErrAgentRetired", loadErr)
	}

	assertRealDriverSkills(t, newRealDriverController(t, right, declaration), neighborID, agentID, map[string]string{"neighbor-only": "neighbor"})
}

func openRealDriverStore(t *testing.T, opener realDriverOpener) state.StateStore {
	t.Helper()
	st, err := opener()
	if err != nil {
		t.Fatalf("open real StateStore: %v", err)
	}
	t.Cleanup(func() { _ = st.Close(context.Background()) })
	return st
}

func closeRealDriverStore(t *testing.T, st state.StateStore) {
	t.Helper()
	if err := st.Close(t.Context()); err != nil {
		t.Fatalf("close real StateStore: %v", err)
	}
}

func newRealDriverDurable(t *testing.T, st state.StateStore) *sessionoverlay.DurableStore {
	t.Helper()
	personal, err := sessionoverlay.NewDurableStore(st, nil)
	if err != nil {
		t.Fatalf("NewDurableStore: %v", err)
	}
	return personal
}

func newRealDriverCutover(t *testing.T, st state.StateStore, declaration config.SessionPersonalCutoverTenant) *sessionoverlay.CutoverController {
	t.Helper()
	cutover, err := sessionoverlay.NewCutoverController(st, []config.SessionPersonalCutoverTenant{declaration})
	if err != nil {
		t.Fatalf("NewCutoverController: %v", err)
	}
	return cutover
}

func newRealDriverController(t *testing.T, st state.StateStore, declaration config.SessionPersonalCutoverTenant) *sessionoverlay.SessionPersonalController {
	t.Helper()
	controller, err := sessionoverlay.NewSessionPersonalController(newRealDriverDurable(t, st), newRealDriverCutover(t, st, declaration), realDriverLegacyReader{})
	if err != nil {
		t.Fatalf("NewSessionPersonalController: %v", err)
	}
	return controller
}

func runRealDriverPair(t *testing.T, barrier *realDriverTargetBarrier, left, right func() error) [2]error {
	t.Helper()
	t.Cleanup(barrier.releaseAll)
	type result struct {
		index int
		err   error
	}
	results := make(chan result, 2)
	go func() { results <- result{index: 0, err: left()} }()
	go func() { results <- result{index: 1, err: right()} }()
	barrier.wait(t, 2)
	barrier.releaseAll()
	var got [2]error
	for range 2 {
		select {
		case result := <-results:
			got[result.index] = result.err
		case <-time.After(realDriverRaceTimeout):
			t.Fatal("timed out waiting for independent race results")
		}
	}
	return got
}

func runRealDriverBlockedMutation(t *testing.T, barrier *realDriverTargetBarrier, mutation, fence func() error) error {
	t.Helper()
	t.Cleanup(barrier.releaseAll)
	result := make(chan error, 1)
	go func() { result <- mutation() }()
	barrier.wait(t, 1)
	fenceErr := fence()
	barrier.releaseAll()
	if fenceErr != nil {
		t.Fatalf("install concurrent fence: %v", fenceErr)
	}
	select {
	case err := <-result:
		return err
	case <-time.After(realDriverRaceTimeout):
		t.Fatal("timed out waiting for fenced mutation result")
		return nil
	}
}

func assertRealDriverOneWinner(t *testing.T, operation string, results [2]error) int {
	t.Helper()
	winner := -1
	losers := 0
	for i, err := range results {
		switch {
		case err == nil:
			if winner != -1 {
				t.Fatalf("%s produced multiple winners: %v", operation, results)
			}
			winner = i
		case errors.Is(err, state.ErrConditionFailed):
			losers++
		default:
			t.Fatalf("%s result[%d] = %v, want nil or ErrConditionFailed", operation, i, err)
		}
	}
	if winner == -1 || losers != 1 {
		t.Fatalf("%s results = %v, want exactly one winner and one condition failure", operation, results)
	}
	return winner
}

func assertRealDriverSkills(t *testing.T, controller *sessionoverlay.SessionPersonalController, id identity.Quadruple, agentID string, want map[string]string) {
	t.Helper()
	got, err := controller.SessionSkills(t.Context(), id, agentID)
	if err != nil {
		t.Fatalf("SessionSkills(%s/%s) = %v", id.UserID, id.SessionID, err)
	}
	if len(got) != len(want) {
		t.Fatalf("SessionSkills(%s/%s) count = %d, want %d: %+v", id.UserID, id.SessionID, len(got), len(want), got)
	}
	for _, skill := range got {
		description, found := want[strings.ToLower(skill.Name)]
		if !found || skill.Description != description {
			t.Fatalf("SessionSkills(%s/%s) unexpected body = %+v, want %v", id.UserID, id.SessionID, skill, want)
		}
	}
}

func assertRealDriverTargetAbsent(t *testing.T, st state.StateStore, id identity.Quadruple, kind string) {
	t.Helper()
	durableID := identity.Quadruple{Identity: id.Identity}
	if _, err := st.Load(t.Context(), durableID, kind); !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("failed fenced mutation left target record: %v", err)
	}
}

func realDriverSkill(name, description string) skills.Skill {
	return skills.Skill{Name: name, Description: description, Trigger: "when needed", Steps: []string{"do it"}, Origin: skills.OriginGenerated, Scope: skills.ScopeSession}
}

func saveRealDriverLifecycle(t *testing.T, st state.StateStore, tenantID, agentID string, retired bool) {
	t.Helper()
	if err := saveRealDriverLifecycleError(st, tenantID, agentID, retired); err != nil {
		t.Fatalf("save lifecycle: %v", err)
	}
}

func saveRealDriverLifecycleError(st state.StateStore, tenantID, agentID string, retired bool) error {
	q, kind, err := agentcfg.LifecycleSlot(tenantID, agentID)
	if err != nil {
		return err
	}
	revisionID := "active-revision"
	if retired {
		revisionID = ""
	}
	body := []byte(fmt.Sprintf(`{"schema":1,"revision_id":%q,"updated_at":"2026-08-02T00:00:00Z"}`, revisionID))
	return st.Save(context.Background(), state.StateRecord{ID: state.NewEventID(), Identity: q, Kind: kind, Bytes: body})
}

func realDriverRandomSuffix(t *testing.T) string {
	t.Helper()
	var bytes [8]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	return hex.EncodeToString(bytes[:])
}

func realDriverPostgresDSN(t *testing.T) string {
	t.Helper()
	baseDSN := os.Getenv("HARBOR_PG_DSN")
	if baseDSN == "" {
		t.Skip("reason: HARBOR_PG_DSN is unset; Postgres two-instance Phase 233a race coverage runs in CI")
	}
	schema := "harbor_233a_" + realDriverRandomSuffix(t)
	adminDB, err := sql.Open("pgx", baseDSN)
	if err != nil {
		t.Fatalf("open Postgres admin connection: %v", err)
	}
	defer func() { _ = adminDB.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), realDriverRaceTimeout)
	defer cancel()
	if _, err := adminDB.ExecContext(ctx, fmt.Sprintf("CREATE SCHEMA %s", realDriverQuoteIdentifier(schema))); err != nil {
		t.Fatalf("create Postgres test schema: %v", err)
	}
	t.Cleanup(func() {
		dropDB, openErr := sql.Open("pgx", baseDSN)
		if openErr != nil {
			t.Errorf("open Postgres cleanup connection: %v", openErr)
			return
		}
		defer func() { _ = dropDB.Close() }()
		dropCtx, dropCancel := context.WithTimeout(context.Background(), realDriverRaceTimeout)
		defer dropCancel()
		if _, dropErr := dropDB.ExecContext(dropCtx, fmt.Sprintf("DROP SCHEMA %s CASCADE", realDriverQuoteIdentifier(schema))); dropErr != nil {
			t.Errorf("drop Postgres test schema: %v", dropErr)
		}
	})
	return realDriverAppendSearchPath(baseDSN, schema)
}

func realDriverQuoteIdentifier(value string) string {
	return `"` + strings.ReplaceAll(value, `"`, `""`) + `"`
}

func realDriverAppendSearchPath(dsn, schema string) string {
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err == nil {
			query := u.Query()
			option := "-c search_path=" + schema
			if existing := query.Get("options"); existing != "" {
				option = existing + " " + option
			}
			query.Set("options", option)
			u.RawQuery = query.Encode()
			return u.String()
		}
	}
	return dsn + " options='-c search_path=" + schema + "'"
}
