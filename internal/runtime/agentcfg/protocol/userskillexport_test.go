package protocol_test

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	agentcfgprotocol "github.com/hurtener/Harbor/internal/runtime/agentcfg/protocol"
	"github.com/hurtener/Harbor/internal/skills"
	skillpkg "github.com/hurtener/Harbor/internal/skills/package"
)

func exportRequest(agent, name string) prototypes.AgentConfigUserSkillsExportRequest {
	return prototypes.AgentConfigUserSkillsExportRequest{AgentID: agent, Name: name}
}

func exportArchiveEntries(t *testing.T, data []byte) map[string][]byte {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("read exported zip: %v", err)
	}
	out := make(map[string][]byte, len(zr.File))
	for _, file := range zr.File {
		if file.Method != zip.Store {
			t.Fatalf("exported entry %q uses compression that can amplify on import", file.Name)
		}
		if _, exists := out[file.Name]; exists {
			t.Fatalf("duplicate exported entry %q", file.Name)
		}
		r, err := file.Open()
		if err != nil {
			t.Fatal(err)
		}
		body, readErr := io.ReadAll(r)
		closeErr := r.Close()
		if readErr != nil || closeErr != nil {
			t.Fatalf("read exported entry: %v, close: %v", readErr, closeErr)
		}
		out[file.Name] = body
	}
	return out
}

func skillExportMutationCounts(fx *importFixture) [10]int {
	put, upsert, del, delAgent, delInstalled, restore := fx.store.counts()
	save, saveIf, deleteIf, deleteState := fx.proposals.stateCounts()
	return [10]int{put, upsert, del, delAgent, delInstalled, restore, save, saveIf, deleteIf, deleteState}
}

func TestUserSkillExport_RoundTrip_SupportsAndMarkdownOnly(t *testing.T) {
	for _, markdownOnly := range []bool{false, true} {
		t.Run(fmt.Sprintf("markdown_only=%t", markdownOnly), func(t *testing.T) {
			fx := newImportFixture(t)
			ctx := importCtx(importUserA, testAgentID)
			md := "---\nname: Demo-Skill\ndescription: 'Reference guide: use this when asked'\n---\n# Overview\nA free-form instruction.\n"
			entries := map[string]string{"SKILL.md": md}
			if !markdownOnly {
				entries["SKILL.md"] += "\nRead [reference](docs/reference.md), [script](scripts/check.py), and ![image](assets/icon.svg).\n"
				entries["docs/reference.md"] = "# Reference\nContent that survives installation.\n"
				entries["scripts/check.py"] = "print('reference data only')\n"
				entries["assets/icon.svg"] = "<svg xmlns=\"http://www.w3.org/2000/svg\" width=\"1\" height=\"1\"></svg>"
			}
			var artifactID string
			if markdownOnly {
				ref, err := fx.artifacts.PutBytes(ctx, artifacts.ArtifactScope{TenantID: "t", UserID: "u", SessionID: "s"}, []byte(md), artifacts.PutOpts{Namespace: "import", Filename: "SKILL.md"})
				if err != nil {
					t.Fatal(err)
				}
				artifactID = ref.ID
			} else {
				artifactID = fx.uploadPackage(t, importUserA, entries)
			}
			review, err := fx.svc.Validate(ctx, fx.validateReq(artifactID))
			if err != nil {
				t.Fatalf("validate source: %v", err)
			}
			if _, err := fx.svc.Commit(ctx, commitReqFromValidate(review, false)); err != nil {
				t.Fatalf("commit source: %v", err)
			}
			// The staging artifact is not part of the installed package's lifetime.
			if _, err := fx.artifacts.Delete(ctx, artifacts.ArtifactScope{TenantID: "t", UserID: "u", SessionID: "s"}, artifactID); err != nil {
				t.Fatal(err)
			}
			before, err := fx.store.GetInstalledPackage(ctx, importQuad(importUserA), testAgentID, "demo-skill")
			if err != nil {
				t.Fatal(err)
			}
			counts := skillExportMutationCounts(fx)
			exported, err := fx.svc.Export(ctx, exportRequest(testAgentID, " Demo-Skill "))
			if err != nil {
				t.Fatalf("export: %v", err)
			}
			if exported.PackageHash != review.PackageHash || exported.Name != "demo-skill" || exported.SizeBytes <= 0 || exported.SizeBytes != exported.Artifact.SizeBytes || exported.ProtocolVersion != prototypes.ProtocolVersion {
				t.Fatalf("export metadata mismatch: %+v", exported)
			}
			if exported.Artifact.MimeType != "application/zip" || exported.Artifact.Filename != "demo-skill.zip" || exported.Artifact.Scope != (prototypes.ArtifactScope{Tenant: "t", User: "u", Session: "s"}) {
				t.Fatalf("export artifact metadata: %+v", exported.Artifact)
			}
			data, found, err := fx.artifacts.Get(ctx, artifacts.ArtifactScope{TenantID: "t", UserID: "u", SessionID: "s"}, exported.Artifact.ID)
			if err != nil || !found {
				t.Fatalf("read export: found=%t err=%v", found, err)
			}
			files := exportArchiveEntries(t, data)
			if len(files) != len(entries) {
				t.Fatalf("export file count=%d want=%d", len(files), len(entries))
			}
			for path, source := range entries {
				if path != "SKILL.md" && string(files[path]) != source {
					t.Fatalf("support bytes changed: %s", path)
				}
			}
			for _, forbidden := range []string{"scope:", "agent_id:", "tenant:", "user:", "artifact://", "skillpkg://"} {
				if bytes.Contains(files["SKILL.md"], []byte(forbidden)) {
					t.Fatalf("portable document includes %q", forbidden)
				}
			}
			repeated, err := fx.svc.Export(ctx, exportRequest(testAgentID, "demo-skill"))
			if err != nil || repeated.Artifact.ID != exported.Artifact.ID || repeated.Artifact.SHA256 != exported.Artifact.SHA256 {
				t.Fatalf("non-deterministic export: %+v err=%v", repeated, err)
			}
			otherAgent := "export-target-agent"
			validated, err := fx.svc.Validate(importCtx(importUserA, otherAgent), agentcfgprotocol.UserSkillImportValidateRequest{AgentID: otherAgent, ArtifactID: exported.Artifact.ID})
			if err != nil || validated.PackageHash != review.PackageHash {
				t.Fatalf("export → other agent validate: hash=%q err=%v", validated.PackageHash, err)
			}
			after, err := fx.store.GetInstalledPackage(ctx, importQuad(importUserA), testAgentID, "demo-skill")
			if err != nil || !reflect.DeepEqual(before, after) || skillExportMutationCounts(fx) != counts {
				t.Fatalf("export mutated source/configuration: err=%v", err)
			}
		})
	}
}

func TestUserSkillExport_Isolation_DurableSourceSessionOwnedArtifact(t *testing.T) {
	fx := newImportFixture(t)
	_, _, err := fx.validateCommit(t, importUserA, map[string]string{"SKILL.md": importSkillMD}, false)
	if err != nil {
		t.Fatal(err)
	}
	// A same-name ephemeral row never masks the durable package.
	sessionSkill := skills.Skill{Name: "demo-skill", AgentID: testAgentID, Trigger: "session", Steps: []string{"session only"}, Scope: skills.ScopeSession, Origin: skills.OriginGenerated}
	if err := fx.store.Upsert(context.Background(), importQuad(importUserA), sessionSkill); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		id    identity.Identity
		agent string
	}{
		{"foreign-user", identity.Identity{TenantID: "t", UserID: "foreign", SessionID: "s"}, testAgentID},
		{"foreign-tenant", identity.Identity{TenantID: "foreign", UserID: "u", SessionID: "s"}, testAgentID},
		{"foreign-agent", importUserA, "other-agent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := fx.svc.Export(importCtx(tc.id, tc.agent), exportRequest(tc.agent, "demo-skill")); !errors.Is(err, skills.ErrInstalledPackageNotFound) {
				t.Fatalf("foreign source export: %v", err)
			}
		})
	}
	otherSession := importUserA
	otherSession.SessionID = "other-session"
	exported, err := fx.svc.Export(importCtx(otherSession, testAgentID), exportRequest(testAgentID, "demo-skill"))
	if err != nil {
		t.Fatalf("durable export from next session: %v", err)
	}
	for _, id := range []identity.Identity{importUserA, {TenantID: "t", UserID: "foreign", SessionID: "other-session"}, {TenantID: "foreign", UserID: "u", SessionID: "other-session"}} {
		_, found, err := fx.artifacts.Get(context.Background(), artifacts.ArtifactScope{TenantID: id.TenantID, UserID: id.UserID, SessionID: id.SessionID}, exported.Artifact.ID)
		if err != nil || found {
			t.Fatalf("export artifact escaped its exact session: id=%+v found=%t err=%v", id, found, err)
		}
	}
}

type exportReadSpy struct {
	skills.SkillStore
	reads atomic.Int64
	unit  *skills.InstalledPackage
}

func (s *exportReadSpy) GetInstalledPackage(ctx context.Context, q identity.Quadruple, agent, name string) (skills.InstalledPackage, error) {
	s.reads.Add(1)
	if s.unit != nil {
		return *s.unit, nil
	}
	return s.SkillStore.GetInstalledPackage(ctx, q, agent, name)
}

func TestUserSkillExport_Admission_RefusesBeforePackageRead(t *testing.T) {
	fx := newImportFixture(t)
	spy := &exportReadSpy{SkillStore: fx.store.SkillStore}
	fx.store.SkillStore = spy
	unverified, _ := identity.With(context.Background(), importUserA)
	base := importCtx(importUserA, testAgentID)
	canceled, cancel := context.WithCancel(base)
	cancel()
	for _, tc := range []struct {
		name string
		ctx  context.Context
		req  prototypes.AgentConfigUserSkillsExportRequest
		want error
	}{
		{"unverified", unverified, exportRequest(testAgentID, "demo-skill"), agentcfgprotocol.ErrUserSkillImportIdentityRequired},
		{"agent-reach", auth.WithAgentReach(base, []string{"other"}), exportRequest(testAgentID, "demo-skill"), agentcfgprotocol.ErrUserSkillImportAgentReachDenied},
		{"session-reach", auth.WithSessionReach(base, []string{"other"}), exportRequest(testAgentID, "demo-skill"), agentcfgprotocol.ErrUserSkillImportSessionReachDenied},
		{"forged-owner", base, prototypes.AgentConfigUserSkillsExportRequest{Identity: prototypes.IdentityScope{User: "other"}, AgentID: testAgentID, Name: "demo-skill"}, agentcfgprotocol.ErrUserSkillImportIdentityRequired},
		{"missing-name", base, exportRequest(testAgentID, ""), agentcfgprotocol.ErrIdentityRequired},
		{"canceled", canceled, exportRequest(testAgentID, "demo-skill"), context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := fx.svc.Export(tc.ctx, tc.req); !errors.Is(err, tc.want) {
				t.Fatalf("export refusal=%v want=%v", err, tc.want)
			}
		})
	}
	if spy.reads.Load() != 0 {
		t.Fatalf("unauthorized requests read package data: %d", spy.reads.Load())
	}
	fx.assertZeroMutations(t)
	fx.assertZeroStateWrites(t)
}

func TestUserSkillExport_Limits_RefusesBeforeArtifactWrite(t *testing.T) {
	fx := newImportFixture(t)
	_, _, err := fx.validateCommit(t, importUserA, map[string]string{"SKILL.md": importSkillMD}, false)
	if err != nil {
		t.Fatal(err)
	}
	ctx := importCtx(importUserA, testAgentID)
	unit, err := fx.store.GetInstalledPackage(ctx, importQuad(importUserA), testAgentID, "demo-skill")
	if err != nil {
		t.Fatal(err)
	}
	// Each canonical field remains within its limit, but the synthesized
	// document exceeds the independent SKILL.md byte ceiling.
	items := make([]string, skillpkg.MaxPackageSteps)
	for i := range items {
		items[i] = strings.Repeat("😀", skillpkg.MaxPackageTextRunes)
	}
	unit.Skill.Steps, unit.Package.Skill.Steps = items, items
	unit.Skill.Preconditions, unit.Package.Skill.Preconditions = items, items
	unit.Skill.FailureModes, unit.Package.Skill.FailureModes = items, items
	unit.Skill.ContentHash = skills.CanonicalContentHash(unit.Skill)
	unit.PackageHash, err = skillpkg.PackageHash(unit.Package)
	if err != nil {
		t.Fatal(err)
	}
	if err := skills.ValidateInstalledPackage(unit); err != nil {
		t.Fatalf("oversize document fixture must remain a valid installed DTO: %v", err)
	}
	fx.store.SkillStore = &exportReadSpy{SkillStore: fx.store.SkillStore, unit: &unit}
	before, err := fx.artifacts.List(ctx, artifacts.ArtifactScope{TenantID: "t", UserID: "u", SessionID: "s"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fx.svc.Export(ctx, exportRequest(testAgentID, "demo-skill")); !errors.Is(err, agentcfgprotocol.ErrUserSkillImportPackageInvalid) {
		t.Fatalf("oversize document export: %v", err)
	}
	after, err := fx.artifacts.List(ctx, artifacts.ArtifactScope{TenantID: "t", UserID: "u", SessionID: "s"})
	if err != nil || len(before) != len(after) {
		t.Fatalf("refused export wrote an artifact: before=%d after=%d err=%v", len(before), len(after), err)
	}
}

func TestUserSkillExport_ConcurrentReuse_IsolatedAndCancelIndependent(t *testing.T) {
	fx := newImportFixture(t)
	baseline := runtime.NumGoroutine()
	for _, user := range []identity.Identity{importUserA, {TenantID: "t", UserID: "other-user", SessionID: "s"}} {
		md := strings.ReplaceAll(importSkillMD, "A demo skill body.", "Owned by "+user.UserID+".")
		if _, _, err := fx.validateCommit(t, user, map[string]string{"SKILL.md": md}, false); err != nil {
			t.Fatal(err)
		}
	}
	counts := skillExportMutationCounts(fx)
	const n = 100
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := importUserA
			if i%2 != 0 {
				id.UserID = "other-user"
			}
			id.SessionID = fmt.Sprintf("export-session-%d", i)
			ctx, cancel := context.WithCancel(importCtx(id, testAgentID))
			defer cancel()
			if i%5 == 0 {
				cancel()
			}
			out, err := fx.svc.Export(ctx, exportRequest(testAgentID, "demo-skill"))
			if i%5 == 0 {
				if !errors.Is(err, context.Canceled) {
					errs <- fmt.Errorf("canceled export %d: %w", i, err)
				}
				return
			}
			if err != nil {
				errs <- fmt.Errorf("export %d: %w", i, err)
				return
			}
			if out.Artifact.Scope.User != id.UserID || out.Artifact.Scope.Session != id.SessionID {
				errs <- fmt.Errorf("export %d crossed owner scope", i)
				return
			}
			checked, err := fx.svc.Validate(ctx, agentcfgprotocol.UserSkillImportValidateRequest{AgentID: testAgentID, ArtifactID: out.Artifact.ID})
			if err != nil || !strings.Contains(checked.Review.Description, "Owned by "+id.UserID+".") || checked.PackageHash != out.PackageHash {
				errs <- fmt.Errorf("export %d crossed package content: %w", i, err)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if skillExportMutationCounts(fx) != counts {
		t.Fatal("concurrent export mutated skill/configuration/proposal state")
	}
	var after int
	for range 50 {
		after = runtime.NumGoroutine()
		if after <= baseline+2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if after > baseline+2 {
		t.Fatalf("goroutine leak: baseline=%d after=%d", baseline, after)
	}
}

func TestUserSkillExport_Lifecycle_RetiredAgentRefused(t *testing.T) {
	fx := newImportFixture(t)
	retirePreviewAgent(t, fx.reg, testAgentID)
	spy := &exportReadSpy{SkillStore: fx.store.SkillStore}
	fx.store.SkillStore = spy
	if _, err := fx.svc.Export(importCtx(importUserA, testAgentID), exportRequest(testAgentID, "demo-skill")); !errors.Is(err, agentcfg.ErrAgentRetired) {
		t.Fatalf("retired export: %v", err)
	}
	if spy.reads.Load() != 0 {
		t.Fatal("retired export read package data")
	}
}
