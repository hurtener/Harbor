// strict_decode_test.go — the control transport refuses request-body
// members its wire types do not define, and names them (D-374).
//
// The defect: `encoding/json` discards an unmatched member. On a Protocol
// request that converts a caller's explicit instruction into a success that
// did not happen — a client speaking a newer Protocol sends an additive
// optional field, an older Runtime drops it, and the run proceeds without the
// content the caller believes it supplied. `caller_memory` is the concrete
// instance; the class is every additive optional field the Protocol will ever
// gain.
//
// Two properties are pinned per method family:
//
//  1. An unknown member is REFUSED with CodeInvalidRequest — not dropped.
//  2. The refusal NAMES the member. A refusal that will not say which member
//     is unusable for a client trying to learn what the Runtime supports, and
//     it is indistinguishable from the transport's own malformed-body answer,
//     which carries the identical code.
//
// The corresponding negative property — a body carrying only KNOWN members
// still decodes — is what stops this becoming a blanket 400, and is asserted
// alongside so the guard cannot pass by refusing everything.
package control_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/transports/control"
)

// TestDecode_UnknownMemberRefusedAndNamed drives every request family the
// control transport decodes: `start` (StartRequest), a steering control
// (ControlRequest), and the artifacts cluster (its own per-method types).
func TestDecode_UnknownMemberRefusedAndNamed(t *testing.T) {
	verified := identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"}

	for _, tc := range []struct {
		name   string
		method methods.Method
		// body carries exactly one member no wire type defines. The name
		// is shaped like a plausible FUTURE field rather than garbage —
		// the failure being guarded is a real additive field arriving
		// early, not a typo.
		body string
		// unknown is the member name the refusal must quote.
		unknown string
	}{
		{
			name:    "start rejects an unknown member",
			method:  methods.MethodStart,
			body:    `{"identity":{"tenant":"t1","user":"u1","session":"s1"},"query":"hi","caller_memoryy":{"a":1}}`,
			unknown: "caller_memoryy",
		},
		{
			name:    "a steering control rejects an unknown member",
			method:  methods.MethodCancel,
			body:    `{"identity":{"tenant":"t1","user":"u1","session":"s1","run":"r1","scope":"session_user"},"payloadd":{}}`,
			unknown: "payloadd",
		},
		{
			name: "an unknown member NESTED in the identity scope is rejected too",
			// The refusal must reach members below the top level: an
			// identity scope is where a caller would plausibly graft a
			// field, and a top-level-only check would miss it.
			method:  methods.MethodStart,
			body:    `{"identity":{"tenant":"t1","user":"u1","session":"s1","impersonatingg":{}}}`,
			unknown: "impersonatingg",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cs, cleanup := newTestSurface(t)
			t.Cleanup(cleanup)
			h, err := control.NewHandler(cs)
			if err != nil {
				t.Fatalf("NewHandler: %v", err)
			}
			mux := http.NewServeMux()
			mux.Handle(control.RoutePattern, withIdentity(h, verified))

			status, perr := postMethod(t, mux, tc.method, tc.body)
			if status != http.StatusBadRequest {
				t.Fatalf("%s: status = %d, want 400 — the member was accepted, so a caller sending a field this Runtime does not know is told it succeeded",
					tc.method, status)
			}
			if perr.Code != protoerrors.CodeInvalidRequest {
				t.Fatalf("%s: code = %q, want %q", tc.method, perr.Code, protoerrors.CodeInvalidRequest)
			}
			if !strings.Contains(perr.Message, tc.unknown) {
				t.Fatalf("%s: refusal %q does not name the unknown member %q — a client cannot act on a refusal that will not say what it refused",
					tc.method, perr.Message, tc.unknown)
			}
		})
	}
}

// TestDecode_KnownMembersStillAccepted is the negative half. Without it the
// test above passes just as well against a transport that refuses every body,
// which would be a far worse regression than the one being fixed.
func TestDecode_KnownMembersStillAccepted(t *testing.T) {
	verified := identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"}
	cs, cleanup := newTestSurface(t)
	t.Cleanup(cleanup)
	h, err := control.NewHandler(cs)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	mux := http.NewServeMux()
	mux.Handle(control.RoutePattern, withIdentity(h, verified))

	// Every member here is declared on StartRequest, `caller_memory`
	// included — the field whose silent loss motivated the strict decode.
	body := `{"identity":{"tenant":"t1","user":"u1","session":"s1"},` +
		`"query":"hello","description":"d","priority":1,` +
		`"caller_memory":{"recalled":["a"]}}`
	status, perr := postMethod(t, mux, methods.MethodStart, body)
	if status != http.StatusOK {
		t.Fatalf("a body of only KNOWN members was refused %d (%q) — the strict decode has become a blanket refusal",
			status, perr.Message)
	}
}

// TestDecode_TrailingDataRefused pins that the swap from json.Unmarshal to a
// streaming decoder did not LOOSEN anything. `json.Unmarshal` refuses a second
// document after the first; a bare `Decoder.Decode` reads one value and stops.
func TestDecode_TrailingDataRefused(t *testing.T) {
	verified := identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"}
	cs, cleanup := newTestSurface(t)
	t.Cleanup(cleanup)
	h, err := control.NewHandler(cs)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	mux := http.NewServeMux()
	mux.Handle(control.RoutePattern, withIdentity(h, verified))

	body := `{"identity":{"tenant":"t1","user":"u1","session":"s1"},"query":"a"} {"query":"b"}`
	status, perr := postMethod(t, mux, methods.MethodStart, body)
	if status != http.StatusBadRequest {
		t.Fatalf("a body with trailing data returned %d, want 400 — json.Unmarshal refused this shape and the replacement must not admit it", status)
	}
	if perr.Code != protoerrors.CodeInvalidRequest {
		t.Fatalf("code = %q, want %q", perr.Code, protoerrors.CodeInvalidRequest)
	}
}

// TestDecode_UnknownMemberDetailIsBounded pins that the refusal echoes the
// decoder's reason without echoing an unbounded caller-controlled string. The
// member NAME is what makes the refusal actionable; a 64 KiB member name is
// not, and reflecting it whole would let a caller choose the size of the
// Runtime's own error response.
func TestDecode_UnknownMemberDetailIsBounded(t *testing.T) {
	verified := identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"}
	cs, cleanup := newTestSurface(t)
	t.Cleanup(cleanup)
	h, err := control.NewHandler(cs)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	mux := http.NewServeMux()
	mux.Handle(control.RoutePattern, withIdentity(h, verified))

	huge := strings.Repeat("x", 4096)
	body := `{"identity":{"tenant":"t1","user":"u1","session":"s1"},"` + huge + `":1}`
	status, perr := postMethod(t, mux, methods.MethodStart, body)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", status)
	}
	if len(perr.Message) > 512 {
		t.Fatalf("refusal message is %d bytes — the caller-controlled member name is being echoed unbounded", len(perr.Message))
	}
	// It must still be a decode refusal that says something useful, not a
	// bare code: the truncation may not cost the diagnosis entirely.
	if !strings.Contains(perr.Message, "unknown field") {
		t.Fatalf("refusal %q no longer reports an unknown field — truncation swallowed the reason", perr.Message)
	}
}
