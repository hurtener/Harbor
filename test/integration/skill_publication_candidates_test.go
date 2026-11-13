package integration_test

import (
	"archive/zip"
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"

	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
)

// Crosses the authenticated HTTP mux, the production personal-skill service,
// the durable skill driver and the StateStore-backed publication and
// candidate stores: a person reads their own skill, submits it for review,
// an admin reads and accepts it, and another person reads the published
// body. A skill with support files is refused, other people cannot see or
// read the candidate, and only an admin can accept.
func TestE2E_SkillReadsAndPublicationCandidates(t *testing.T) {
	s := newAgentReachStack(t, withPersonalPackageImports(t))
	agent := s.stack.AgentConfigID
	owner := arIdentity("candidate-owner")
	ownerToken := arToken(t, s, owner, nil, true, []string{agent})
	ownerScope := prototypes.IdentityScope{Tenant: owner.TenantID, User: owner.UserID, Session: owner.SessionID}
	admin := arIdentity("candidate-admin")
	admin.UserID = "candidate-admin-user"
	adminToken := arToken(t, s, admin, []string{"admin"}, true, []string{agent})
	adminScope := prototypes.IdentityScope{Tenant: admin.TenantID, User: admin.UserID, Session: admin.SessionID}
	other := arIdentity("candidate-other")
	other.UserID = "candidate-other-user"
	otherToken := arToken(t, s, other, nil, true, []string{agent})
	otherScope := prototypes.IdentityScope{Tenant: other.TenantID, User: other.UserID, Session: other.SessionID}

	status, raw := arPost(t, s, "/v1/control/runtime.info", ownerToken, map[string]any{})
	arAssertStatus(t, status, raw, http.StatusOK)
	for _, capability := range []string{"skill_reads", "skill_publication_candidates"} {
		if !strings.Contains(string(raw), `"`+capability+`"`) {
			t.Fatalf("runtime.info does not advertise %s: %s", capability, raw)
		}
	}

	// A personal skill without support files.
	status, raw = arPost(t, s, "/v1/agent_config/user/skills/upsert", ownerToken, prototypes.AgentConfigUserSkillsUpsertRequest{
		Identity: ownerScope, AgentID: agent,
		Skill: prototypes.AgentConfigSkillInput{Name: "expense-check", Description: "Checks **expenses**", Trigger: "when expenses arrive", Steps: []string{"Compare totals"}, Origin: "generated", Scope: "user"},
	})
	arAssertStatus(t, status, raw, http.StatusOK)

	status, raw = arPost(t, s, "/v1/agent_config/user/skills/read", ownerToken, prototypes.AgentConfigUserSkillsReadRequest{Identity: ownerScope, AgentID: agent, Name: "expense-check"})
	arAssertStatus(t, status, raw, http.StatusOK)
	read := arDecode[prototypes.AgentConfigUserSkillsReadResponse](t, raw)
	if read.Name != "expense-check" || !strings.Contains(read.Document, "Compare totals") || len(read.SupportFiles) != 0 {
		t.Fatalf("personal read: %+v", read)
	}
	status, raw = arPost(t, s, "/v1/agent_config/user/skills/read", otherToken, prototypes.AgentConfigUserSkillsReadRequest{Identity: otherScope, AgentID: agent, Name: "expense-check"})
	arAssertStatus(t, status, raw, http.StatusNotFound)

	// A package with support files reads its manifest and is refused as a candidate.
	installPackage(t, s, ownerToken, ownerScope, agent)
	status, raw = arPost(t, s, "/v1/agent_config/user/skills/read", ownerToken, prototypes.AgentConfigUserSkillsReadRequest{Identity: ownerScope, AgentID: agent, Name: "with-reference", SupportPath: "docs/reference.md"})
	arAssertStatus(t, status, raw, http.StatusOK)
	pkg := arDecode[prototypes.AgentConfigUserSkillsReadResponse](t, raw)
	if len(pkg.SupportFiles) != 1 || pkg.SupportFile == nil || !strings.Contains(pkg.SupportFile.Text, "Exact support bytes") {
		t.Fatalf("package read: %+v", pkg)
	}
	status, raw = arPost(t, s, "/v1/control/skills.publications.candidates.submit", ownerToken, prototypes.SkillPublicationCandidateSubmitRequest{Identity: ownerScope, AgentID: agent, Name: "with-reference", IdempotencyKey: "submit-ref"})
	arAssertStatus(t, status, raw, http.StatusUnprocessableEntity)
	arAssertCode(t, raw, "candidate_support_files_unsupported")

	status, raw = arPost(t, s, "/v1/control/skills.publications.candidates.submit", ownerToken, prototypes.SkillPublicationCandidateSubmitRequest{Identity: ownerScope, AgentID: agent, Name: "expense-check", Note: "Saves time", IdempotencyKey: "submit-1"})
	arAssertStatus(t, status, raw, http.StatusOK)
	candidate := arDecode[prototypes.SkillPublicationCandidateSubmitResponse](t, raw).Candidate
	if candidate.State != "pending" || candidate.SubmitterUser != owner.UserID {
		t.Fatalf("candidate: %+v", candidate)
	}
	status, raw = arPost(t, s, "/v1/control/skills.publications.candidates.submit", ownerToken, prototypes.SkillPublicationCandidateSubmitRequest{Identity: ownerScope, AgentID: agent, Name: "expense-check", IdempotencyKey: "submit-2"})
	arAssertStatus(t, status, raw, http.StatusConflict)
	arAssertCode(t, raw, "candidate_pending_exists")

	// Other people neither see nor read it, nor accept it.
	status, raw = arPost(t, s, "/v1/control/skills.publications.candidates.list", otherToken, prototypes.SkillPublicationCandidateListRequest{Identity: otherScope})
	arAssertStatus(t, status, raw, http.StatusOK)
	if got := arDecode[prototypes.SkillPublicationCandidateListResponse](t, raw).Candidates; len(got) != 0 {
		t.Fatalf("other user sees %d candidates", len(got))
	}
	status, raw = arPost(t, s, "/v1/control/skills.publications.candidates.read", otherToken, prototypes.SkillPublicationCandidateReadRequest{Identity: otherScope, CandidateID: candidate.CandidateID})
	arAssertStatus(t, status, raw, http.StatusNotFound)
	acceptReq := prototypes.SkillPublicationCandidateAcceptRequest{Identity: otherScope, CandidateID: candidate.CandidateID, ExpectedGeneration: candidate.Generation, Mode: "new", IdempotencyKey: "accept-1"}
	status, _ = arPost(t, s, "/v1/control/skills.publications.candidates.accept", otherToken, acceptReq)
	if status != http.StatusForbidden && status != http.StatusUnauthorized {
		t.Fatalf("non-admin accept status = %d", status)
	}

	// The admin reviews and accepts.
	status, raw = arPost(t, s, "/v1/control/skills.publications.candidates.list", adminToken, prototypes.SkillPublicationCandidateListRequest{Identity: adminScope, State: "pending"})
	arAssertStatus(t, status, raw, http.StatusOK)
	if got := arDecode[prototypes.SkillPublicationCandidateListResponse](t, raw).Candidates; len(got) != 1 || got[0].CandidateID != candidate.CandidateID {
		t.Fatalf("admin pending list: %+v", got)
	}
	status, raw = arPost(t, s, "/v1/control/skills.publications.candidates.read", adminToken, prototypes.SkillPublicationCandidateReadRequest{Identity: adminScope, CandidateID: candidate.CandidateID})
	arAssertStatus(t, status, raw, http.StatusOK)
	if review := arDecode[prototypes.SkillPublicationCandidateReadResponse](t, raw); !strings.Contains(review.Document, "Compare totals") {
		t.Fatalf("admin review: %+v", review)
	}
	acceptReq.Identity = adminScope
	acceptReq.DecisionNote = "Approved"
	status, raw = arPost(t, s, "/v1/control/skills.publications.candidates.accept", adminToken, acceptReq)
	arAssertStatus(t, status, raw, http.StatusOK)
	accepted := arDecode[prototypes.SkillPublicationCandidateAcceptResponse](t, raw)
	if accepted.Candidate.State != "accepted" || accepted.Publication.Name != "expense-check" || accepted.Candidate.PublicationID != accepted.Publication.PublicationID {
		t.Fatalf("accepted: %+v", accepted)
	}
	status, raw = arPost(t, s, "/v1/control/skills.publications.candidates.accept", adminToken, acceptReq)
	arAssertStatus(t, status, raw, http.StatusOK)
	if replay := arDecode[prototypes.SkillPublicationCandidateAcceptResponse](t, raw); replay.Publication.PublicationID != accepted.Publication.PublicationID {
		t.Fatalf("replay published again: %+v", replay)
	}

	// Anyone in the organization reads the published body.
	status, raw = arPost(t, s, "/v1/control/skills.publications.read", otherToken, prototypes.SkillPublicationReadRequest{Identity: otherScope, PublicationID: accepted.Publication.PublicationID})
	arAssertStatus(t, status, raw, http.StatusOK)
	published := arDecode[prototypes.SkillPublicationReadResponse](t, raw)
	if !strings.Contains(published.Document, "Compare totals") || published.RevisionID != accepted.Publication.RevisionID {
		t.Fatalf("published read: %+v", published)
	}
	status, raw = arPost(t, s, "/v1/control/skills.publications.read", otherToken, prototypes.SkillPublicationReadRequest{Identity: otherScope, PublicationID: "pub_missing"})
	arAssertStatus(t, status, raw, http.StatusNotFound)
}

func installPackage(t *testing.T, s *agentReachStack, token string, scope prototypes.IdentityScope, agent string) {
	t.Helper()
	var archive bytes.Buffer
	zw := zip.NewWriter(&archive)
	for _, file := range []struct{ name, content string }{
		{"SKILL.md", "---\nname: with-reference\ndescription: Uses a reference\n---\n# Instructions\nRead [reference](docs/reference.md).\n"},
		{"docs/reference.md", "# Reference\nExact support bytes.\n"},
	} {
		w, err := zw.Create(file.name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(w, file.content); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	artifactScope := prototypes.ArtifactScope{Tenant: scope.Tenant, User: scope.User, Session: scope.Session}
	status, raw := arPost(t, s, "/v1/control/artifacts.put", token, prototypes.ArtifactsPutRequest{Scope: artifactScope, Bytes: archive.Bytes(), Opts: prototypes.ArtifactsPutOpts{Filename: "with-reference.zip", MimeType: "application/zip"}})
	arAssertStatus(t, status, raw, http.StatusOK)
	uploaded := arDecode[prototypes.ArtifactsPutResponse](t, raw)
	status, raw = arPost(t, s, "/v1/agent_config/user/skills/import_validate", token, prototypes.AgentConfigUserSkillsImportValidateRequest{Identity: scope, AgentID: agent, ArtifactID: uploaded.Ref.ID})
	arAssertStatus(t, status, raw, http.StatusOK)
	review := arDecode[prototypes.AgentConfigUserSkillsImportValidateResponse](t, raw)
	status, raw = arPost(t, s, "/v1/agent_config/user/skills/import_commit", token, prototypes.AgentConfigUserSkillsImportCommitRequest{Identity: scope, AgentID: agent, Name: review.Review.Name, ProposalToken: review.ProposalToken, ReviewedPackageHash: review.PackageHash, ExpectedContentHash: review.ExpectedContentHash})
	arAssertStatus(t, status, raw, http.StatusOK)
}
