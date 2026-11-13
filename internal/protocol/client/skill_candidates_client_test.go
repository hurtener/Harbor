package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hurtener/Harbor/internal/protocol/types"
)

func TestSkillReadAndCandidateClients_UseCanonicalRoutesAndClientIdentity(t *testing.T) {
	wantIdentity := types.IdentityScope{Tenant: "tenant", User: "user", Session: "session"}
	forged := types.IdentityScope{Tenant: "forged", User: "forged", Session: "forged"}
	lastPath := ""
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastPath = r.URL.Path
		var body struct {
			Identity types.IdentityScope `json:"identity"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Identity != wantIdentity {
			t.Errorf("%s identity=%+v err=%v", r.URL.Path, body.Identity, err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"protocol_version":"0.1.0"}`))
	}))
	defer server.Close()
	base := testClient(t, server)
	reads, ok := base.(SkillReadClient)
	if !ok {
		t.Fatal("client does not implement SkillReadClient")
	}
	cands, ok := base.(SkillPublicationCandidatesClient)
	if !ok {
		t.Fatal("client does not implement SkillPublicationCandidatesClient")
	}
	ctx := context.Background()
	tests := []struct {
		path string
		call func() error
	}{
		{"/v1/agent_config/user/skills/read", func() error {
			_, err := reads.AgentConfigUserSkillsRead(ctx, types.AgentConfigUserSkillsReadRequest{Identity: forged, AgentID: "a", Name: "n"})
			return err
		}},
		{"/v1/control/skills.publications.read", func() error {
			_, err := reads.SkillPublicationsRead(ctx, types.SkillPublicationReadRequest{Identity: forged, PublicationID: "p"})
			return err
		}},
		{"/v1/control/skills.publications.candidates.submit", func() error {
			_, err := cands.SkillPublicationCandidatesSubmit(ctx, types.SkillPublicationCandidateSubmitRequest{Identity: forged, AgentID: "a", Name: "n", IdempotencyKey: "k"})
			return err
		}},
		{"/v1/control/skills.publications.candidates.list", func() error {
			_, err := cands.SkillPublicationCandidatesList(ctx, types.SkillPublicationCandidateListRequest{Identity: forged})
			return err
		}},
		{"/v1/control/skills.publications.candidates.read", func() error {
			_, err := cands.SkillPublicationCandidatesRead(ctx, types.SkillPublicationCandidateReadRequest{Identity: forged, CandidateID: "c"})
			return err
		}},
		{"/v1/control/skills.publications.candidates.accept", func() error {
			_, err := cands.SkillPublicationCandidatesAccept(ctx, types.SkillPublicationCandidateAcceptRequest{Identity: forged, CandidateID: "c", Mode: "new", IdempotencyKey: "k"})
			return err
		}},
		{"/v1/control/skills.publications.candidates.reject", func() error {
			_, err := cands.SkillPublicationCandidatesReject(ctx, types.SkillPublicationCandidateRejectRequest{Identity: forged, CandidateID: "c", IdempotencyKey: "k"})
			return err
		}},
		{"/v1/control/skills.publications.candidates.withdraw", func() error {
			_, err := cands.SkillPublicationCandidatesWithdraw(ctx, types.SkillPublicationCandidateWithdrawRequest{Identity: forged, CandidateID: "c", IdempotencyKey: "k"})
			return err
		}},
	}
	for _, tc := range tests {
		if err := tc.call(); err != nil {
			t.Fatalf("%s: %v", tc.path, err)
		}
		if lastPath != tc.path {
			t.Fatalf("path=%q want %q", lastPath, tc.path)
		}
	}
}
