package tokenexchange_test

import (
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
)

type participantSubject struct {
	TenantID     string `json:"tenant_id"`
	UserID       string `json:"user_id"`
	SessionID    string `json:"session_id"`
	SessionOwner string `json:"session_owner,omitempty"`
}

func decodeParticipantSubject(t *testing.T, s string) participantSubject {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		t.Fatalf("decode subject: %v", err)
	}
	var st participantSubject
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatalf("unmarshal subject: %v", err)
	}
	return st
}

// A participant writing in another user's session exchanges as itself,
// names the partition owner for the broker to authorize, and never shares
// a cache entry with either its own sessions or the owner's.
func TestToken_SessionParticipantActsAsWriter(t *testing.T) {
	broker := newFakeBroker(t)
	prov, _, _ := mkProvider(t, broker)
	partition := identity.Identity{TenantID: tDummyTenant, UserID: "owner-user", SessionID: "shared-session"}

	guest, err := identity.WithParticipant(mkCtx(t, partition), identity.Participant{UserID: "writer-user", Speaker: "Writer"})
	if err != nil {
		t.Fatal(err)
	}
	tok, err := prov.Token(guest, "any")
	if err != nil {
		t.Fatalf("participant Token: %v", err)
	}
	if tok.UserID != "writer-user" {
		t.Fatalf("token user = %q, want the writer", tok.UserID)
	}
	got := decodeParticipantSubject(t, broker.form().Get("subject_token"))
	want := participantSubject{TenantID: tDummyTenant, UserID: "writer-user", SessionID: "shared-session", SessionOwner: "owner-user"}
	if got != want {
		t.Fatalf("subject = %+v, want %+v", got, want)
	}

	// The partition owner in the same session: its own subject, no owner field.
	if _, err := prov.Token(mkCtx(t, partition), "any"); err != nil {
		t.Fatal(err)
	}
	if got := decodeParticipantSubject(t, broker.form().Get("subject_token")); got != (participantSubject{TenantID: tDummyTenant, UserID: "owner-user", SessionID: "shared-session"}) {
		t.Fatalf("owner subject = %+v", got)
	}

	// The writer in its own session is a separate cache family.
	if _, err := prov.Token(mkCtx(t, identity.Identity{TenantID: tDummyTenant, UserID: "writer-user", SessionID: "own"}), "any"); err != nil {
		t.Fatal(err)
	}
	if broker.calls() != 3 {
		t.Fatalf("broker calls = %d, want 3 (participant, owner and own-session never share a cached credential)", broker.calls())
	}
	// A repeat participant call in the shared session is served from its own cache.
	if _, err := prov.Token(guest, "any"); err != nil {
		t.Fatal(err)
	}
	if broker.calls() != 3 {
		t.Fatalf("broker calls = %d after a cached participant repeat, want 3", broker.calls())
	}
}

// A labelled owner (participant equal to the partition user) is an ordinary
// subject: byte-identical to an unlabelled call.
func TestToken_LabelledOwnerIsOrdinarySubject(t *testing.T) {
	broker := newFakeBroker(t)
	prov, _, _ := mkProvider(t, broker)
	ctx, err := identity.WithParticipant(mkCtx(t, aliceID()), identity.Participant{UserID: aliceID().UserID, Speaker: "Alice"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := prov.Token(ctx, "any"); err != nil {
		t.Fatal(err)
	}
	if got := decodeParticipantSubject(t, broker.form().Get("subject_token")); got.SessionOwner != "" || got.UserID != aliceID().UserID {
		t.Fatalf("labelled owner subject = %+v", got)
	}
}
