package tasks

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"sync"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/auth"
)

type testAdmissionSealer struct{ aead cipher.AEAD }

func (s testAdmissionSealer) Seal(plain []byte) ([]byte, error) {
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	return s.aead.Seal(nonce, nonce, plain, nil), nil
}

func (s testAdmissionSealer) Open(envelope []byte) ([]byte, error) {
	if len(envelope) < s.aead.NonceSize() {
		return nil, errors.New("short envelope")
	}
	n := s.aead.NonceSize()
	return s.aead.Open(nil, envelope[:n], envelope[n:], nil)
}

func testAgentReachAuthority(t *testing.T) *AgentReachAdmissionAuthority {
	t.Helper()
	block, err := aes.NewCipher(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	authority, err := NewAgentReachAdmissionAuthority(testAdmissionSealer{aead: aead})
	if err != nil {
		t.Fatal(err)
	}
	return authority
}

func TestAgentReachAdmission_SealedCaptureRestoreAndTamperDenial(t *testing.T) {
	id := identity.Identity{TenantID: "tenant", UserID: "user", SessionID: "session"}
	authority := testAgentReachAuthority(t)
	sealed, err := authority.Admit(context.Background(), id, "agent-a")
	if err != nil {
		t.Fatal(err)
	}
	receipt := CaptureAgentReachAdmission(sealed, id, "agent-a")
	if receipt == nil {
		t.Fatal("exact sealed admission was not captured")
	}
	task := &Task{Identity: identity.Quadruple{Identity: id}, AgentID: "agent-a", AgentReachAdmission: receipt}
	raw, err := json.Marshal(task)
	if err != nil {
		t.Fatal(err)
	}
	var restarted Task
	if err := json.Unmarshal(raw, &restarted); err != nil {
		t.Fatal(err)
	}
	restoredCtx, got, admitted := authority.Restore(context.Background(), &restarted)
	if !admitted || got != "agent-a" {
		t.Fatalf("restart restore = (%q, %v), want (agent-a, true)", got, admitted)
	}
	verified, ok := identity.FromVerified(restoredCtx)
	if !ok || verified != id {
		t.Fatalf("restored verified identity=%+v present=%t, want %+v", verified, ok, id)
	}
	reach, ok := auth.AgentReachFrom(restoredCtx)
	if !ok || len(reach) != 1 || reach[0] != "agent-a" {
		t.Fatalf("restored signed reach=%v present=%t, want [agent-a]", reach, ok)
	}

	for _, mutate := range []func(*Task){
		func(v *Task) { v.AgentID = "agent-b" },
		func(v *Task) { v.Identity.UserID = "other-user" },
		func(v *Task) { v.AgentReachAdmission.Envelope[0] ^= 0xff },
		func(v *Task) {
			v.AgentID = "agent-b"
			v.AgentReachAdmission.Envelope = append([]byte(nil), v.AgentReachAdmission.Envelope...)
		},
		func(v *Task) {
			v.AgentID = "agent-b"
			forged, _ := json.Marshal(agentReachAdmissionClaims{
				Schema: agentReachAdmissionSchema, TenantID: id.TenantID, UserID: id.UserID,
				SessionID: id.SessionID, EffectiveAgentID: "agent-b",
			})
			digest := sha256.Sum256(forged)
			v.AgentReachAdmission.BindingDigest = append([]byte(nil), digest[:]...)
		},
	} {
		candidate := restarted
		receiptCopy := AgentReachAdmission{
			Envelope:      append([]byte(nil), restarted.AgentReachAdmission.Envelope...),
			BindingDigest: append([]byte(nil), restarted.AgentReachAdmission.BindingDigest...),
		}
		candidate.AgentReachAdmission = &receiptCopy
		mutate(&candidate)
		if _, _, admitted := authority.Restore(context.Background(), &candidate); admitted {
			t.Fatalf("tampered admission restored: task=%+v receipt=%+v", candidate, candidate.AgentReachAdmission)
		}
	}
	other := id
	other.SessionID = "other-session"
	if got := CaptureAgentReachAdmission(sealed, other, "agent-a"); got != nil {
		t.Fatalf("cross-identity capture returned %+v", got)
	}
	if got := CaptureAgentReachAdmission(sealed, id, "agent-b"); got != nil {
		t.Fatalf("wrong-agent capture returned %+v", got)
	}
}

func TestAgentReachAdmission_IdenticalSubjectResealIsIdempotent(t *testing.T) {
	authority := testAgentReachAuthority(t)
	id := identity.Identity{TenantID: "tenant", UserID: "user", SessionID: "session"}
	ctxA, err := authority.Admit(context.Background(), id, "agent-a")
	if err != nil {
		t.Fatal(err)
	}
	ctxB, err := authority.Admit(context.Background(), id, "agent-a")
	if err != nil {
		t.Fatal(err)
	}
	a := CaptureAgentReachAdmission(ctxA, id, "agent-a")
	b := CaptureAgentReachAdmission(ctxB, id, "agent-a")
	if bytes.Equal(a.Envelope, b.Envelope) {
		t.Fatal("fresh AES-GCM seals unexpectedly reused an envelope")
	}
	if !AgentReachAdmissionsEqual(a, b) {
		t.Fatal("identical authenticated subjects are not idempotency-equal")
	}
}

func TestAgentReachAdmission_ConcurrentCaptureNoBleed(t *testing.T) {
	const n = 128
	authority := testAgentReachAuthority(t)
	var wg sync.WaitGroup
	errs := make(chan string, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := identity.Identity{TenantID: "tenant", UserID: "user-" + strconv.Itoa(i), SessionID: "session-" + strconv.Itoa(i)}
			agent := "agent-" + strconv.Itoa(i)
			ctx, err := authority.Admit(context.Background(), id, agent)
			if err != nil {
				errs <- agent
				return
			}
			receipt := CaptureAgentReachAdmission(ctx, id, agent)
			if receipt == nil {
				errs <- agent
				return
			}
			if _, got, admitted := authority.Restore(context.Background(), &Task{Identity: identity.Quadruple{Identity: id}, AgentID: agent, AgentReachAdmission: receipt}); !admitted || got != agent {
				errs <- agent
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for agent := range errs {
		t.Fatalf("concurrent admission bled at %s", agent)
	}
}
