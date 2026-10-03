package protocol_test

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/transports/control"
	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/runtime/sessionadmission"
	"github.com/hurtener/Harbor/internal/runtime/steering"
	"github.com/hurtener/Harbor/internal/state"
	"github.com/hurtener/Harbor/internal/tasks"
)

const (
	admissionLegacyAudience = "harbor-admission-test"
	admissionScopedAudience = "harbor-admission-test-scoped"
	admissionIssuer         = "https://admission.test"
	admissionCoordinator    = "coordinator-a"
)

type admissionFixtureKeys struct{ public *rsa.PublicKey }

func (k admissionFixtureKeys) KeyByID(kid string) (crypto.PublicKey, string, error) {
	if kid != "admission-test-key" {
		return nil, "", auth.ErrUnknownKey
	}
	return k.public, "RS256", nil
}

type admissionProtocolFixture struct {
	*surfaceFixture
	gate      *sessionadmission.Gate
	validator auth.Validator
	private   *rsa.PrivateKey
	handler   http.Handler
}

func newAdmissionProtocolFixture(t *testing.T) *admissionProtocolFixture {
	t.Helper()
	fx := newSurfaceFixture(t)
	gate, err := sessionadmission.New(fx.state)
	if err != nil {
		t.Fatal(err)
	}
	fx.surface, err = protocol.NewControlSurface(fx.tasks, fx.steering,
		protocol.WithAgentResolver(fixtureAgentResolver{}),
		protocol.WithAgentReachAuthorizer(auth.NewAgentReachAuthorizer()),
		protocol.WithSessionAdmissionGate(gate))
	if err != nil {
		t.Fatal(err)
	}
	// Reuse the documented non-secret auth fixture, never production keys.
	encoded, err := os.ReadFile(filepath.Join("auth", "testdata", "rs256_private.pem"))
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(encoded)
	if block == nil {
		t.Fatal("invalid test key PEM")
	}
	private, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		parsed, parseErr := x509.ParsePKCS8PrivateKey(block.Bytes)
		if parseErr != nil {
			t.Fatal(parseErr)
		}
		var ok bool
		private, ok = parsed.(*rsa.PrivateKey)
		if !ok {
			t.Fatal("test key is not RSA")
		}
	}
	validator, err := auth.NewValidator(admissionFixtureKeys{public: &private.PublicKey},
		auth.WithRedactor(auditpatterns.New()),
		auth.WithClock(func() time.Time { return time.Unix(1800000000, 0) }),
		auth.WithIssuer(admissionIssuer), auth.WithAudience(admissionLegacyAudience),
		auth.WithScopedTokenAudience(admissionScopedAudience))
	if err != nil {
		t.Fatal(err)
	}
	ctrl, err := control.NewHandler(fx.surface)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle(control.RoutePattern, ctrl)
	return &admissionProtocolFixture{surfaceFixture: fx, gate: gate, validator: validator,
		private: private, handler: auth.Middleware(validator)(mux)}
}

func (f *admissionProtocolFixture) token(t *testing.T, id identity.Identity, reach []methods.Method, epoch uint64) string {
	t.Helper()
	claims := jwt.MapClaims{
		"iss": admissionIssuer, "sub": admissionCoordinator,
		"aud": admissionLegacyAudience, "exp": int64(1800003600),
		"tenant": id.TenantID, "user": id.UserID, "session": id.SessionID,
		"scopes":             []string{string(auth.ScopeAdmin)},
		auth.AgentReachClaim: []string{"fixture-default"},
	}
	if reach != nil {
		claims["aud"] = admissionScopedAudience
		names := make([]string, len(reach))
		for i, method := range reach {
			names[i] = string(method)
		}
		claims[auth.MethodReachClaim] = names
	}
	if epoch > 0 {
		claims[auth.SessionAdmissionEpochClaim] = epoch
		claims[auth.SessionAdmissionCoordinatorClaim] = admissionCoordinator
	}
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	token.Header["kid"] = "admission-test-key"
	signed, err := token.SignedString(f.private)
	if err != nil {
		t.Fatal(err)
	}
	return signed
}

func (f *admissionProtocolFixture) verifiedContext(t *testing.T, token string) context.Context {
	t.Helper()
	verified, err := f.validator.Validate(t.Context(), token)
	if err != nil {
		t.Fatal(err)
	}
	ctx, err := identity.WithVerified(t.Context(), verified.Identity)
	if err != nil {
		t.Fatal(err)
	}
	ctx = auth.WithScopes(ctx, verified.Scopes)
	ctx = auth.WithAgentReach(ctx, verified.AgentReach)
	ctx = auth.WithTokenAuthority(ctx, auth.TokenAuthority{Issuer: verified.Issuer, Subject: verified.Subject})
	ctx = auth.WithSessionAdmission(ctx, verified.SessionAdmission)
	if verified.MethodReach != nil {
		ctx = auth.WithMethodReach(ctx, verified.MethodReach)
	}
	if verified.SessionReach != nil {
		ctx = auth.WithSessionReach(ctx, verified.SessionReach)
	}
	return sessionadmission.WithGate(ctx, f.gate)
}

func (f *admissionProtocolFixture) request(t *testing.T, token string, method methods.Method, body any) *httptest.ResponseRecorder {
	t.Helper()
	return f.requestInSession(t, token, method, body, "")
}

func (f *admissionProtocolFixture) requestInSession(t *testing.T, token string, method methods.Method, body any, selected string) *httptest.ResponseRecorder {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/control/"+string(method), bytes.NewReader(encoded))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	if selected != "" {
		req.Header.Set(auth.HeaderSession, selected)
	}
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	return rec
}

func admissionControlRequest(run identity.Quadruple, event string) *types.ControlRequest {
	return &types.ControlRequest{Identity: types.IdentityScope{
		Tenant: run.TenantID, User: run.UserID, Session: run.SessionID, Run: run.RunID,
	}, EventID: event}
}

func assertAdmissionHTTP(t *testing.T, response *httptest.ResponseRecorder, status int) {
	t.Helper()
	if response.Code != status {
		t.Fatalf("HTTP = %d, want %d: %s", response.Code, status, response.Body.String())
	}
	if status == http.StatusForbidden {
		var failure protoerrors.Error
		if err := json.Unmarshal(response.Body.Bytes(), &failure); err != nil || failure.Code != protoerrors.CodeScopeMismatch {
			t.Fatalf("expected scope_mismatch: %s (%v)", response.Body.String(), err)
		}
	}
}

func TestSessionAdmission_HTTP_SameBroadTokenBeforeAndAfterEnrollment(t *testing.T) {
	f := newAdmissionProtocolFixture(t)
	run := testRun("admission-http-run")
	inbox, err := f.steering.Open(run)
	if err != nil {
		t.Fatal(err)
	}
	broad := f.token(t, run.Identity, nil, 0)
	assertAdmissionHTTP(t, f.request(t, broad, methods.MethodPause, admissionControlRequest(run, "before-enrollment")), http.StatusOK)
	if inbox.Len() != 1 {
		t.Fatal("broad token before enrollment did not reach the real steering inbox")
	}
	if _, err := inbox.Drain(); err != nil {
		t.Fatal(err)
	}
	admin := f.verifiedContext(t, broad)
	if _, err := f.gate.Enroll(admin, run.Identity, 0, 1); err != nil {
		t.Fatal(err)
	}
	// Reuse the exact same signed string, including its existing admin scope.
	assertAdmissionHTTP(t, f.request(t, broad, methods.MethodPause, admissionControlRequest(run, "after-enrollment")), http.StatusForbidden)
	if inbox.Len() != 0 {
		t.Fatal("old broad bearer wrote after enrollment")
	}
	current := f.token(t, run.Identity, []methods.Method{methods.MethodPause}, 1)
	assertAdmissionHTTP(t, f.request(t, current, methods.MethodPause, admissionControlRequest(run, "current-authority")), http.StatusOK)
	if inbox.Len() != 1 {
		t.Fatal("current scoped authority did not reach the existing control path")
	}
	if _, err := inbox.Drain(); err != nil {
		t.Fatal(err)
	}
	if _, err := f.gate.Enroll(admin, run.Identity, 1, 2); err != nil {
		t.Fatal(err)
	}
	assertAdmissionHTTP(t, f.request(t, current, methods.MethodPause, admissionControlRequest(run, "stale-authority")), http.StatusForbidden)
	if inbox.Len() != 0 {
		t.Fatal("stale scoped authority wrote after epoch advance")
	}
}

func TestSessionAdmission_HTTP_NativeControlsRequireExplicitMethodReach(t *testing.T) {
	for _, tc := range []struct {
		method methods.Method
		kind   steering.ControlType
	}{
		{methods.MethodApprove, steering.ControlApprove},
		{methods.MethodReject, steering.ControlReject},
		{methods.MethodResume, steering.ControlResume},
	} {
		t.Run(string(tc.method), func(t *testing.T) {
			f := newAdmissionProtocolFixture(t)
			run := testRun("native-" + string(tc.method))
			inbox, err := f.steering.Open(run)
			if err != nil {
				t.Fatal(err)
			}
			broad := f.token(t, run.Identity, nil, 0)
			if _, err := f.gate.Enroll(f.verifiedContext(t, broad), run.Identity, 0, 1); err != nil {
				t.Fatal(err)
			}
			reader := f.token(t, run.Identity, []methods.Method{methods.MethodEventsSubscribe}, 1)
			assertAdmissionHTTP(t, f.request(t, reader, tc.method, admissionControlRequest(run, "reader-denied")), http.StatusForbidden)
			if inbox.Len() != 0 {
				t.Fatal("reader credential reached native control inbox")
			}
			current := f.token(t, run.Identity, []methods.Method{tc.method}, 1)
			assertAdmissionHTTP(t, f.request(t, current, tc.method, admissionControlRequest(run, "native-accepted")), http.StatusOK)
			events, err := inbox.Drain()
			if err != nil || len(events) != 1 || events[0].Type != tc.kind {
				t.Fatalf("native control event = %#v, %v", events, err)
			}
			// Explicit admission to one native verb grants no sibling verb.
			other := methods.MethodResume
			if other == tc.method {
				other = methods.MethodApprove
			}
			assertAdmissionHTTP(t, f.request(t, current, other, admissionControlRequest(run, "sibling-denied")), http.StatusForbidden)
			if inbox.Len() != 0 {
				t.Fatal("unlisted sibling native control reached inbox")
			}
		})
	}
}

func TestSessionAdmission_DirectDispatchCannotBypassMethodOrEpoch(t *testing.T) {
	f := newAdmissionProtocolFixture(t)
	run := testRun("direct-admission-run")
	inbox, err := f.steering.Open(run)
	if err != nil {
		t.Fatal(err)
	}
	broad := f.token(t, run.Identity, nil, 0)
	broadCtx := f.verifiedContext(t, broad)
	if _, err := f.gate.Enroll(broadCtx, run.Identity, 0, 1); err != nil {
		t.Fatal(err)
	}
	reader := f.token(t, run.Identity, []methods.Method{methods.MethodEventsSubscribe, methods.MethodArtifactsGet}, 1)
	readerCtx := f.verifiedContext(t, reader)
	for _, tc := range []struct {
		name string
		ctx  context.Context
	}{
		{"reader method denial", readerCtx},
		{"old broad epoch denial", broadCtx},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := f.surface.Dispatch(tc.ctx, methods.MethodResume, admissionControlRequest(run, tc.name))
			if code := codeOf(t, err); code != protoerrors.CodeScopeMismatch {
				t.Fatalf("direct control denial = %s", code)
			}
		})
	}
	if inbox.Len() != 0 {
		t.Fatal("direct caller bypassed admission to reach steering")
	}
	// Existing Apps dependencies are never invoked: the real AppsSurface's
	// transport-independent dispatch must refuse reader method reach first.
	invoker := &stubInvoker{}
	apps := newAppsSurface(t, &stubResourceReader{}, invoker)
	_, err = apps.Dispatch(readerCtx, methods.MethodMCPAppsCallTool, &types.MCPAppCallToolRequest{
		Identity: types.IdentityScope{Tenant: run.TenantID, User: run.UserID, Session: run.SessionID},
		ServerID: "server-a", Tool: "example-tool",
	})
	if code := codeOf(t, err); code != protoerrors.CodeScopeMismatch {
		t.Fatalf("reader Apps callback denial = %s", code)
	}
	if invoker.gotTool != "" || invoker.admittedCalls != 0 {
		t.Fatal("reader authority reached App invocation")
	}
}

func TestSessionAdmission_Start_ConcurrentExactKeyReplay(t *testing.T) {
	f := newAdmissionProtocolFixture(t)
	id := testRun("").Identity
	broad := f.token(t, id, nil, 0)
	if _, err := f.gate.Enroll(f.verifiedContext(t, broad), id, 0, 1); err != nil {
		t.Fatal(err)
	}
	current := f.token(t, id, []methods.Method{methods.MethodStart}, 1)
	ctx := f.verifiedContext(t, current)
	const count = 2
	responses := make([]*types.StartResponse, count)
	failures := make([]error, count)
	ready := make(chan struct{})
	var wg sync.WaitGroup
	for i := range count {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-ready
			// Each caller owns its request object; only immutable authority
			// and the actual compiled runtime surface are shared.
			request := &types.StartRequest{
				Identity: types.IdentityScope{Tenant: id.TenantID, User: id.UserID, Session: id.SessionID},
				Query:    "one accepted task", IdempotencyKey: "admission-exact-replay",
			}
			result, err := f.surface.Dispatch(ctx, methods.MethodStart, request)
			failures[i] = err
			if err == nil {
				responses[i] = result.(*types.StartResponse)
			}
		}(i)
	}
	close(ready)
	wg.Wait()
	for _, err := range failures {
		if err != nil {
			t.Fatalf("concurrent exact keyed Start: %v", err)
		}
	}
	if responses[0].TaskID == "" || responses[0].TaskID != responses[1].TaskID || responses[0].Reused == responses[1].Reused {
		t.Fatalf("expected one Start and one exact replay: %#v, %#v", responses[0], responses[1])
	}
	stored, err := f.tasks.List(ctx, id, tasks.TaskFilter{})
	if err != nil || len(stored) != 1 {
		t.Fatalf("real task registry after keyed replay = %#v, %v", stored, err)
	}
}

func TestSessionAdmission_RestrictedMutationRequiresAssembledGate(t *testing.T) {
	fx := newSurfaceFixture(t)
	id := testRun("").Identity
	ctx := auth.WithMethodReach(authCtx(t, id), []methods.Method{methods.MethodStart})
	ctx = auth.WithSessionAdmission(ctx, &auth.SessionAdmissionAuthority{Epoch: 1, Coordinator: admissionCoordinator, Identity: id})
	_, err := fx.surface.Dispatch(ctx, methods.MethodStart, &types.StartRequest{
		Identity: types.IdentityScope{Tenant: id.TenantID, User: id.UserID, Session: id.SessionID},
		Query:    "must not spawn without durable gate",
	})
	if code := codeOf(t, err); code != protoerrors.CodeRuntimeError {
		t.Fatalf("unassembled restricted mutation = %s", code)
	}
	stored, err := fx.tasks.List(ctx, id, tasks.TaskFilter{})
	if err != nil || len(stored) != 0 {
		t.Fatalf("unassembled mutation created a real task: %#v, %v", stored, err)
	}
}

func TestSessionAdmission_EnrollmentRejectsUnaddressableTokenOwner(t *testing.T) {
	f := newAdmissionProtocolFixture(t)
	for _, tc := range []struct {
		name        string
		issuer      string
		coordinator string
	}{
		{"empty coordinator", admissionIssuer, ""},
		{"oversize coordinator", admissionIssuer, strings.Repeat("a", 129)},
		{"spaced coordinator", admissionIssuer, "coordinator a"},
		{"control coordinator", admissionIssuer, "coordinator\x00a"},
		{"empty issuer", "", admissionCoordinator},
		{"oversize issuer", strings.Repeat("a", 2049), admissionCoordinator},
		{"spaced issuer", "https://admission.test ", admissionCoordinator},
		{"control issuer", "https://admission.test\n", admissionCoordinator},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := testRun("").Identity
			id.SessionID = tc.name
			ctx := auth.WithTokenAuthority(authCtx(t, id, auth.ScopeAdmin), auth.TokenAuthority{Issuer: tc.issuer, Subject: tc.coordinator})
			if _, err := f.gate.Enroll(ctx, id, 0, 1); !errors.Is(err, sessionadmission.ErrDenied) {
				t.Fatalf("unaddressable owner enrollment = %v", err)
			}
			policy, err := f.gate.PolicyFor(ctx, id)
			if err != nil || policy.Epoch != 0 {
				t.Fatalf("rejected owner changed durable policy: %#v, %v", policy, err)
			}
		})
	}
}

func TestSessionAdmission_UncertainAcceptedEffectKeepsEnrollmentBlocked(t *testing.T) {
	f := newAdmissionProtocolFixture(t)
	id := testRun("").Identity
	ctx := f.verifiedContext(t, f.token(t, id, nil, 0))
	_, err := sessionadmission.Run(ctx, id, methods.MethodStart, func(accepted context.Context) (bool, error) {
		// The real registry durably accepts a task before its caller loses
		// certainty about the outcome. This is the boundary a storage or
		// transport error after commitment can expose in production.
		_, spawnErr := f.tasks.Spawn(accepted, tasks.SpawnRequest{
			Identity: identity.Quadruple{Identity: id}, Kind: tasks.KindForeground,
			Query: "accepted before outcome became uncertain",
		})
		if spawnErr != nil {
			t.Fatal(spawnErr)
		}
		return false, errors.New("outcome unknown after accepted mutation")
	})
	if err == nil {
		t.Fatal("uncertain operation reported success")
	}
	stored, err := f.tasks.List(ctx, id, tasks.TaskFilter{})
	if err != nil || len(stored) != 1 {
		t.Fatalf("uncertain fixture must contain one real accepted task: %#v, %v", stored, err)
	}
	if _, err := f.gate.Enroll(ctx, id, 0, 1); !errors.Is(err, sessionadmission.ErrBusy) {
		t.Fatalf("unknown outcome must keep epoch transition blocked: %v", err)
	}
}

func TestSessionAdmission_ReleasesOnlyKnownRejections(t *testing.T) {
	f := newAdmissionProtocolFixture(t)
	domain := errors.New("validated before acceptance")
	if sessionadmission.Rejected(nil) != nil || !errors.Is(sessionadmission.Rejected(domain), domain) {
		t.Fatal("Rejected must preserve nil and underlying error identity")
	}
	for i, tc := range []struct {
		name     string
		err      error
		released bool
	}{
		{"success", nil, true},
		{"explicit rejection", sessionadmission.Rejected(domain), true},
		{"wrapped explicit rejection", fmt.Errorf("adapter: %w", sessionadmission.Rejected(domain)), true},
		{"canonical invalid request", protoerrors.New(protoerrors.CodeInvalidRequest, "validation rejected"), true},
		{"canonical scope mismatch", protoerrors.New(protoerrors.CodeScopeMismatch, "authority rejected"), true},
		{"joined known rejections", errors.Join(sessionadmission.Rejected(domain), protoerrors.New(protoerrors.CodeRevisionConflict, "compare failed")), true},
		{"unknown domain error", domain, false},
		{"runtime error", protoerrors.New(protoerrors.CodeRuntimeError, "runtime outcome uncertain"), false},
		{"cancellation", context.Canceled, false},
		{"unknown commit", state.ErrCommitOutcomeUnknown, false},
		{"joined unknown and rejection", errors.Join(protoerrors.New(protoerrors.CodeInvalidRequest, "rejected part"), state.ErrCommitOutcomeUnknown), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := testRun("").Identity
			id.SessionID = fmt.Sprintf("rejection-case-%d", i)
			ctx := f.verifiedContext(t, f.token(t, id, nil, 0))
			_, err := sessionadmission.Run(ctx, id, methods.MethodStart, func(context.Context) (bool, error) { return tc.err == nil, tc.err })
			if tc.err != nil && !errors.Is(err, tc.err) {
				t.Fatalf("operation error identity changed: %v, want %v", err, tc.err)
			}
			_, enrollErr := f.gate.Enroll(ctx, id, 0, 1)
			if tc.released && enrollErr != nil || !tc.released && !errors.Is(enrollErr, sessionadmission.ErrBusy) {
				t.Fatalf("released=%v, enrollment=%v", tc.released, enrollErr)
			}
		})
	}
}

func TestSessionAdmission_HTTP_SignedIdentityCannotFallBackToUnenrolledSibling(t *testing.T) {
	f := newAdmissionProtocolFixture(t)
	runA := testRun("admission-bound-run")
	runB := testRun("admission-unenrolled-run")
	runB.SessionID = "unenrolled-sibling"
	inbox, err := f.steering.Open(runB)
	if err != nil {
		t.Fatal(err)
	}
	broad := f.token(t, runA.Identity, nil, 0)
	if _, err := f.gate.Enroll(f.verifiedContext(t, broad), runA.Identity, 0, 1); err != nil {
		t.Fatal(err)
	}
	current := f.token(t, runA.Identity, []methods.Method{methods.MethodPause}, 1)
	// Header/body agree on B, so ordinary middleware/body reconciliation
	// succeeds. The signed admission for A itself must prohibit this write.
	assertAdmissionHTTP(t, f.requestInSession(t, current, methods.MethodPause, admissionControlRequest(runB, "foreign-unenrolled"), runB.SessionID), http.StatusForbidden)
	if inbox.Len() != 0 {
		t.Fatal("signed admission escaped to an unenrolled sibling")
	}
}

func TestSessionAdmission_DirectSignedAuthorityRequiresExistingEnrollment(t *testing.T) {
	f := newAdmissionProtocolFixture(t)
	id := testRun("").Identity
	credential := f.token(t, id, []methods.Method{methods.MethodStart}, 1)
	ctx := f.verifiedContext(t, credential)
	request := &types.StartRequest{Identity: types.IdentityScope{Tenant: id.TenantID, User: id.UserID, Session: id.SessionID}, Query: "must match enrolled epoch"}
	if _, err := f.surface.Dispatch(ctx, methods.MethodStart, request); codeOf(t, err) != protoerrors.CodeScopeMismatch {
		t.Fatalf("signed admission accepted without enrollment: %v", err)
	}
	stored, err := f.tasks.List(ctx, id, tasks.TaskFilter{})
	if err != nil || len(stored) != 0 {
		t.Fatalf("unenrolled authority spawned task: %#v, %v", stored, err)
	}
	// A method-only restriction still preserves the documented unenrolled
	// behavior; only a present signed admission requires matching enrollment.
	methodOnly := f.verifiedContext(t, f.token(t, id, []methods.Method{methods.MethodStart}, 0))
	if _, err := f.surface.Dispatch(methodOnly, methods.MethodStart, request); err != nil {
		t.Fatalf("method-only token lost unenrolled compatibility: %v", err)
	}
}

func TestSessionAdmission_AdmissionOnlyContextCannotBypassMissingGate(t *testing.T) {
	id := testRun("").Identity
	ctx := auth.WithSessionAdmission(authCtx(t, id), &auth.SessionAdmissionAuthority{Epoch: 1, Coordinator: admissionCoordinator, Identity: id})
	called := false
	_, err := sessionadmission.Run(ctx, id, methods.MethodStart, func(context.Context) (bool, error) { called = true; return true, nil })
	if called || codeOf(t, err) != protoerrors.CodeRuntimeError {
		t.Fatalf("authority without gate reached callback=%v, err=%v", called, err)
	}
}

func TestSessionAdmission_DurableRecordRejectsImpossibleOrFutureSemantics(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(map[string]any)
		extra  bool
	}{
		{"owned epoch zero", func(r map[string]any) { r["policy"].(map[string]any)["epoch"] = float64(0) }, false},
		{"replay without pending", func(r map[string]any) { r["replay_key"] = "orphaned-digest" }, false},
		{"method without pending", func(r map[string]any) { r["method"] = string(methods.MethodStart) }, false},
		{"pending without method", func(r map[string]any) { r["pending"] = "pending-acceptance" }, false},
		{"unsupported schema", func(r map[string]any) { r["schema"] = float64(2) }, false},
		{"unknown root field", func(r map[string]any) { r["future_authority"] = true }, false},
		{"unknown policy field", func(r map[string]any) { r["policy"].(map[string]any)["future_authority"] = true }, false},
		{"unknown identity field", func(r map[string]any) {
			r["policy"].(map[string]any)["identity"].(map[string]any)["future_scope"] = "other"
		}, false},
		{"trailing JSON value", nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAdmissionProtocolFixture(t)
			id := testRun("").Identity
			ctx := f.verifiedContext(t, f.token(t, id, nil, 0))
			if _, err := f.gate.Enroll(ctx, id, 0, 1); err != nil {
				t.Fatal(err)
			}
			q := identity.InternalCoordinationQuadruple()
			rows, err := f.state.ListKindForIdentityBounded(ctx, q, state.InternalKindPrefix+"session-admission/v1/", 2)
			if err != nil || len(rows) != 1 {
				t.Fatalf("expected one durable admission record: %#v, %v", rows, err)
			}
			var raw map[string]any
			if err := json.Unmarshal(rows[0].Bytes, &raw); err != nil {
				t.Fatal(err)
			}
			if tc.mutate != nil {
				tc.mutate(raw)
			}
			corrupt, err := json.Marshal(raw)
			if err != nil {
				t.Fatal(err)
			}
			if tc.extra {
				corrupt = append(corrupt, []byte(" {}")...)
			}
			if err := f.state.SaveIf(ctx, []state.SlotExpectation{state.InternalSlotExpectation(q, rows[0].Kind, rows[0].ID)},
				state.NewInternalRecord(state.NewEventID(), q, rows[0].Kind, corrupt)); err != nil {
				t.Fatal(err)
			}
			if _, err := f.gate.PolicyFor(ctx, id); !errors.Is(err, sessionadmission.ErrCorrupt) {
				t.Fatalf("impossible/future record accepted: %v", err)
			}
			called := false
			_, err = sessionadmission.Run(ctx, id, methods.MethodStart, func(context.Context) (bool, error) { called = true; return true, nil })
			if called || codeOf(t, err) != protoerrors.CodeRuntimeError {
				t.Fatalf("corrupt record permitted callback=%v, error=%v", called, err)
			}
		})
	}
}

func TestSessionAdmission_UserMessage_ConcurrentExactReceiptReplay(t *testing.T) {
	f := newAdmissionProtocolFixture(t)
	id := testRun("").Identity
	broad := f.token(t, id, nil, 0)
	broadCtx := f.verifiedContext(t, broad)
	handle, err := f.tasks.Spawn(broadCtx, tasks.SpawnRequest{Identity: identity.Quadruple{Identity: id}, Kind: tasks.KindForeground})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.tasks.MarkRunning(broadCtx, handle.ID); err != nil {
		t.Fatal(err)
	}
	run := identity.Quadruple{Identity: id, RunID: string(handle.ID)}
	inbox, err := f.steering.Open(run)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.gate.Enroll(broadCtx, id, 0, 1); err != nil {
		t.Fatal(err)
	}
	ctx := f.verifiedContext(t, f.token(t, id, []methods.Method{methods.MethodUserMessage}, 1))
	const count = 2
	responses := make([]*types.ControlResponse, count)
	failures := make([]error, count)
	ready := make(chan struct{})
	var wg sync.WaitGroup
	for i := range count {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-ready
			request := admissionControlRequest(run, "exact-admitted-input")
			request.Payload = map[string]any{"message": "one clarified input"}
			result, err := f.surface.Dispatch(ctx, methods.MethodUserMessage, request)
			failures[i] = err
			if err == nil {
				responses[i] = result.(*types.ControlResponse)
			}
		}(i)
	}
	close(ready)
	wg.Wait()
	for _, err := range failures {
		if err != nil {
			t.Fatalf("concurrent exact input replay: %v", err)
		}
	}
	if !responses[0].Accepted || !responses[1].Accepted || responses[0].Receipt == nil || responses[1].Receipt == nil ||
		responses[0].Receipt.InputRevision != responses[1].Receipt.InputRevision {
		t.Fatalf("exact input receipts diverged: %#v, %#v", responses[0], responses[1])
	}
	if inbox.Len() != 1 {
		t.Fatalf("exact input replay queued %d controls, want one", inbox.Len())
	}
	stored, err := f.tasks.Get(ctx, handle.ID)
	if err != nil || len(stored.InputReceipts) != 1 {
		t.Fatalf("authoritative task must hold one input receipt: %#v, %v", stored, err)
	}
}

func TestSessionAdmission_PendingAcceptanceDoesNotExposeStateToRevokedAuthority(t *testing.T) {
	f := newAdmissionProtocolFixture(t)
	id := testRun("").Identity
	broad := f.verifiedContext(t, f.token(t, id, nil, 0))
	if _, err := f.gate.Enroll(broad, id, 0, 1); err != nil {
		t.Fatal(err)
	}
	current := f.verifiedContext(t, f.token(t, id, []methods.Method{methods.MethodStart}, 1))
	_, acceptance, err := sessionadmission.Begin(current, id, methods.MethodStart)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := acceptance.Finish(t.Context()); err != nil {
			t.Error(err)
		}
	})
	stale := f.verifiedContext(t, f.token(t, id, []methods.Method{methods.MethodStart}, 2))
	for _, candidate := range []context.Context{broad, stale} {
		if _, _, err := sessionadmission.Begin(candidate, id, methods.MethodStart); !errors.Is(err, sessionadmission.ErrDenied) {
			t.Fatalf("revoked credential learned pending state instead of being denied: %v", err)
		}
	}
	foreign := auth.WithTokenAuthority(broad, auth.TokenAuthority{Issuer: admissionIssuer, Subject: "different-coordinator"})
	if _, err := f.gate.Enroll(foreign, id, 0, 1); !errors.Is(err, sessionadmission.ErrDenied) {
		t.Fatalf("foreign coordinator learned pending enrollment state: %v", err)
	}
	// A same-owner retry of an already-installed epoch remains observable;
	// it makes no new transition and claims no quiescence.
	if _, err := f.gate.Enroll(broad, id, 0, 1); err != nil {
		t.Fatalf("same-owner exact retry: %v", err)
	}
}

func TestSessionAdmission_DirectIncompleteTargetKeepsIdentityRequired(t *testing.T) {
	f := newAdmissionProtocolFixture(t)
	id := testRun("").Identity
	ctx := f.verifiedContext(t, f.token(t, id, []methods.Method{methods.MethodStart}, 0))
	_, err := f.surface.Dispatch(ctx, methods.MethodStart, &types.StartRequest{Identity: types.IdentityScope{Tenant: id.TenantID, Session: id.SessionID}, Query: "invalid owner"})
	if codeOf(t, err) != protoerrors.CodeIdentityRequired {
		t.Fatalf("incomplete target lost canonical identity-required response: %v", err)
	}
	if _, err := f.surface.Dispatch(ctx, methods.MethodStart, &types.StartRequest{Identity: types.IdentityScope{Tenant: id.TenantID, User: id.UserID, Session: id.SessionID}, Query: "valid owner"}); err != nil {
		t.Fatalf("invalid target latched valid session: %v", err)
	}
}
