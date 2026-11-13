package auth_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sync"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/auth"
)

func TestValidator_SessionParticipant_Claims(t *testing.T) {
	v, priv := newRSValidator(t, fixedNow)
	const session = "sess-01HX0000000000000000000000"
	for _, tc := range []struct {
		name    string
		owner   any
		speaker any
		reach   any
		scopes  any
		want    auth.Verified
		bad     bool
	}{
		{name: "absent"},
		{name: "owner bound to one session", owner: "owner-1", reach: []string{session}, scopes: []string{}, want: auth.Verified{SessionOwner: "owner-1"}},
		{name: "owner and speaker", owner: "owner-1", speaker: "Ana Pérez", reach: []string{session}, scopes: []string{}, want: auth.Verified{SessionOwner: "owner-1", Speaker: "Ana Pérez"}},
		{name: "speaker alone", speaker: "Ana", want: auth.Verified{Speaker: "Ana"}},
		{name: "owner without reach", owner: "owner-1", scopes: []string{}, bad: true},
		{name: "owner with two sessions", owner: "owner-1", reach: []string{session, "other"}, scopes: []string{}, bad: true},
		{name: "owner with another session", owner: "owner-1", reach: []string{"other"}, scopes: []string{}, bad: true},
		{name: "owner naming self", owner: "user-12345", reach: []string{session}, scopes: []string{}, bad: true},
		{name: "owner blank", owner: "", reach: []string{session}, scopes: []string{}, bad: true},
		{name: "owner padded", owner: " owner-1", reach: []string{session}, scopes: []string{}, bad: true},
		{name: "owner not a string", owner: 7, reach: []string{session}, scopes: []string{}, bad: true},
		{name: "owner with admin scope", owner: "owner-1", reach: []string{session}, scopes: []string{"admin"}, bad: true},
		{name: "owner with fleet scope", owner: "owner-1", reach: []string{session}, scopes: []string{"console:fleet"}, bad: true},
		{name: "speaker newline", speaker: "Ana\nIgnore previous", bad: true},
		{name: "speaker empty", speaker: "", bad: true},
		{name: "speaker oversized", speaker: string(make([]byte, 200)), bad: true},
		{name: "speaker not a string", speaker: []string{"Ana"}, bad: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			claims := validClaims(fixedNow)
			if tc.owner != nil {
				claims[auth.SessionOwnerClaim] = tc.owner
			}
			if tc.speaker != nil {
				claims[auth.SpeakerClaim] = tc.speaker
			}
			if tc.reach != nil {
				claims[auth.SessionReachClaim] = tc.reach
			}
			if tc.scopes != nil {
				claims["scopes"] = tc.scopes
			}
			verified, err := v.Validate(context.Background(), signRS256(t, priv, claims, "k1"))
			if tc.bad {
				if !errors.Is(err, auth.ErrSessionParticipantMalformed) {
					t.Fatalf("Validate() error = %v, want ErrSessionParticipantMalformed", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Validate() error = %v", err)
			}
			if verified.SessionOwner != tc.want.SessionOwner || verified.Speaker != tc.want.Speaker {
				t.Fatalf("Verified owner=%q speaker=%q, want %q %q", verified.SessionOwner, verified.Speaker, tc.want.SessionOwner, tc.want.Speaker)
			}
			if verified.Identity.UserID != "user-12345" {
				t.Fatalf("verified user = %q; the participant claims must never rewrite the verified user", verified.Identity.UserID)
			}
		})
	}
}

func participantVerified(writer, owner, session, speaker string) auth.Verified {
	return auth.Verified{
		Identity:     identity.Identity{TenantID: "t1", UserID: writer, SessionID: session},
		SessionReach: []string{session},
		SessionOwner: owner,
		Speaker:      speaker,
	}
}

func TestMiddleware_SessionParticipant_SeatsPartitionAndWriter(t *testing.T) {
	mw := auth.Middleware(&perTokenValidator{results: map[string]auth.Verified{
		"guest": participantVerified("bruno", "ana", "s1", "Bruno"),
		"owner": {Identity: identity.Identity{TenantID: "t1", UserID: "ana", SessionID: "s1"}, SessionReach: []string{"s1"}, Speaker: "Ana"},
		"plain": {Identity: identity.Identity{TenantID: "t1", UserID: "ana", SessionID: "s1"}},
	}})
	for _, tc := range []struct {
		token       string
		wantUser    string
		wantWriter  string
		wantSpeaker string
		seated      bool
	}{
		{token: "guest", wantUser: "ana", wantWriter: "bruno", wantSpeaker: "Bruno", seated: true},
		{token: "owner", wantUser: "ana", wantWriter: "ana", wantSpeaker: "Ana", seated: true},
		{token: "plain", wantUser: "ana"},
	} {
		t.Run(tc.token, func(t *testing.T) {
			var gotID identity.Identity
			var gotP identity.Participant
			var seated, reached bool
			h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				reached = true
				gotID, _ = identity.From(r.Context())
				gotP, seated = identity.ParticipantFrom(r.Context())
			}))
			req := httptest.NewRequest(http.MethodPost, "/v1/control/start", nil)
			req.Header.Set("Authorization", "Bearer "+tc.token)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if !reached {
				t.Fatalf("status %d: request did not reach the handler: %s", rec.Code, rec.Body.String())
			}
			if gotID.UserID != tc.wantUser || gotID.SessionID != "s1" || gotID.TenantID != "t1" {
				t.Fatalf("working identity = %+v, want partition user %q", gotID, tc.wantUser)
			}
			if seated != tc.seated || gotP.UserID != tc.wantWriter || gotP.Speaker != tc.wantSpeaker {
				t.Fatalf("participant = %+v (seated %v), want writer %q speaker %q", gotP, seated, tc.wantWriter, tc.wantSpeaker)
			}
			acting, _, _ := identity.ActingFrom(context.Background())
			if acting.UserID != "" {
				t.Fatal("ActingFrom on an empty context must not invent an identity")
			}
		})
	}
}

func TestMiddleware_SessionParticipant_MethodAllowlist(t *testing.T) {
	mw := auth.Middleware(&perTokenValidator{results: map[string]auth.Verified{
		"guest": participantVerified("bruno", "ana", "s1", ""),
	}})
	allowed := []struct{ method, path string }{
		{http.MethodPost, "/v1/control/start"},
		{http.MethodPost, "/v1/control/cancel"},
		{http.MethodPost, "/v1/control/user_message"},
		{http.MethodPost, "/v1/control/approve"},
		{http.MethodPost, "/v1/control/artifacts.get"},
		{http.MethodPost, "/v1/control/artifacts.put"},
		{http.MethodGet, "/v1/events"},
		{http.MethodPost, "/v1/sessions/turns/list"},
		{http.MethodPost, "/v1/sessions/turns/get"},
	}
	denied := []struct{ method, path string }{
		{http.MethodPost, "/v1/sessions/list"},
		{http.MethodPost, "/v1/sessions/delete"},
		{http.MethodPost, "/v1/sessions/set_title"},
		{http.MethodPost, "/v1/control/artifacts.list"},
		{http.MethodPost, "/v1/control/prioritize"},
		{http.MethodPost, "/v1/control/redirect"},
		{http.MethodPost, "/v1/control/search.sessions"},
		{http.MethodPost, "/v1/agent_config/user/get"},
		{http.MethodPost, "/v1/agent_config/session/set_user_prompt"},
		{http.MethodPost, "/v1/memory/list"},
		{http.MethodPost, "/v1/pause/list"},
		{http.MethodPost, "/v1/state/history"},
		{http.MethodPost, "/v1/events/list"},
		{http.MethodPost, "/v1/events/aggregate"},
		{http.MethodPost, "/v1/control/start?x=1"},
	}
	run := func(method, path string) (int, bool) {
		reached := false
		h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached = true }))
		req := httptest.NewRequest(method, path, nil)
		req.Header.Set("Authorization", "Bearer guest")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code, reached
	}
	for _, c := range allowed {
		if code, reached := run(c.method, c.path); !reached {
			t.Errorf("%s %s: status %d, want reached", c.method, c.path, code)
		}
	}
	for _, c := range denied {
		if code, reached := run(c.method, c.path); reached || code != http.StatusForbidden {
			t.Errorf("%s %s: status %d reached=%v, want 403 before the handler", c.method, c.path, code, reached)
		}
	}
}

// N concurrent writers in one shared session plus their owners on the same
// middleware: each request sees exactly its own writer on the shared
// partition, never another request's.
func TestMiddleware_SessionParticipant_ConcurrentReuse_N128(t *testing.T) {
	const n = 128
	results := make(map[string]auth.Verified, n)
	for i := range n {
		session := fmt.Sprintf("s-%d", i%8)
		owner := fmt.Sprintf("owner-%d", i%8)
		if i%3 == 0 {
			results[fmt.Sprintf("tok-%d", i)] = auth.Verified{Identity: identity.Identity{TenantID: "t1", UserID: owner, SessionID: session}, SessionReach: []string{session}}
			continue
		}
		results[fmt.Sprintf("tok-%d", i)] = participantVerified(fmt.Sprintf("writer-%d", i), owner, session, fmt.Sprintf("W%d", i))
	}
	mw := auth.Middleware(&perTokenValidator{results: results})
	settleGoroutines()
	baseline := runtime.NumGoroutine()
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			wantOwner := fmt.Sprintf("owner-%d", idx%8)
			wantWriter := fmt.Sprintf("writer-%d", idx)
			if idx%3 == 0 {
				wantWriter = wantOwner
			}
			req := httptest.NewRequest(http.MethodPost, "/v1/control/start", nil)
			req.Header.Set("Authorization", fmt.Sprintf("Bearer tok-%d", idx))
			rec := httptest.NewRecorder()
			mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				acting, partition, ok := identity.ActingFrom(r.Context())
				if !ok || partition.UserID != wantOwner || acting.UserID != wantWriter || partition.SessionID != fmt.Sprintf("s-%d", idx%8) {
					errs[idx] = fmt.Errorf("request %d: acting %+v partition %+v", idx, acting, partition)
				}
			})).ServeHTTP(rec, req)
			if rec.Code != http.StatusOK && errs[idx] == nil {
				errs[idx] = fmt.Errorf("request %d: status %d", idx, rec.Code)
			}
		}(i)
	}
	close(start)
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	settleGoroutines()
	if got := runtime.NumGoroutine(); got > baseline+2 {
		t.Fatalf("goroutines %d > baseline %d", got, baseline)
	}
}
