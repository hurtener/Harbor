package auth

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/hurtener/Harbor/internal/identity"
)

// SessionOwnerClaim is the optional signed claim naming the user whose
// partition owns the bearer's one session. It is present only when that
// owner differs from the bearer's own `user`: the bearer then acts as a
// participant inside another user's session of the same tenant. The issuer
// is the sole authority deciding who may participate; Harbor binds the
// grant to exactly one signed session and a closed set of exact-session
// methods.
const SessionOwnerClaim = "session_owner"

// SpeakerClaim is the optional signed display label of the bearer's user.
// It labels the turns the bearer writes so a model sharing one session with
// several people can tell who wrote each message.
const SpeakerClaim = "speaker"

// ErrSessionParticipantMalformed rejects a bearer whose session_owner or
// speaker claim is not well-formed, whose session_owner is not bound to
// exactly one session, or whose participant grant carries an elevated scope.
var ErrSessionParticipantMalformed = errors.New("auth: session participant claim malformed")

// parseSessionParticipant validates the two optional participant claims
// against the already-verified identity and session reach. Absence of both
// keys yields empty strings. A present non-string, blank, oversized or
// self-naming session_owner fails closed, as does a malformed speaker.
func parseSessionParticipant(claims map[string]any, id identity.Identity, sessionReach []string, scopes []Scope) (owner, speaker string, err error) {
	if raw, present := claims[SessionOwnerClaim]; present {
		s, ok := raw.(string)
		if !ok || strings.TrimSpace(s) != s || s == "" || len(s) > 256 || s == id.UserID || s == identity.ReservedControlPlaneUser {
			return "", "", fmt.Errorf("%w: session_owner", ErrSessionParticipantMalformed)
		}
		if len(sessionReach) != 1 || sessionReach[0] != id.SessionID {
			return "", "", fmt.Errorf("%w: session_owner requires session_reach naming exactly the token session", ErrSessionParticipantMalformed)
		}
		for _, sc := range scopes {
			if sc == ScopeAdmin || sc == ScopeConsoleFleet {
				return "", "", fmt.Errorf("%w: a participant bearer cannot carry an elevated scope", ErrSessionParticipantMalformed)
			}
		}
		owner = s
	}
	if raw, present := claims[SpeakerClaim]; present {
		s, ok := raw.(string)
		if !ok || s == "" || identity.ValidateSpeaker(s) != nil {
			return "", "", fmt.Errorf("%w: speaker", ErrSessionParticipantMalformed)
		}
		speaker = s
	}
	return owner, speaker, nil
}

// participantRoutes is the closed set of exact-session requests a
// participant bearer (one carrying session_owner) may make. Everything else
// fails closed before any handler runs: session listing, titles, deletion,
// configuration, memory, search, pause listing and cross-session reads all
// resolve against the owner's whole partition and are not part of a
// participant grant.
var participantRoutes = map[string]struct{}{
	"POST /v1/control/start":         {},
	"POST /v1/control/cancel":        {},
	"POST /v1/control/pause":         {},
	"POST /v1/control/resume":        {},
	"POST /v1/control/approve":       {},
	"POST /v1/control/reject":        {},
	"POST /v1/control/user_message":  {},
	"POST /v1/control/artifacts.get": {},
	"POST /v1/control/artifacts.put": {},
	"GET /v1/events":                 {},
	"POST /v1/sessions/turns/list":   {},
	"POST /v1/sessions/turns/get":    {},
	// D-510: the Apps the shared session rendered. Each handler re-checks
	// that the App is on this session's ledger before reading or calling.
	"POST /v1/control/mcp.servers.read_resource": {},
	"POST /v1/control/mcp.apps.call_tool":        {},
	"POST /v1/control/mcp.apps.tool_context":     {},
}

// participantRouteAllowed reports whether a participant bearer may make r.
func participantRouteAllowed(r *http.Request) bool {
	if r.URL.RawQuery != "" && r.Method != http.MethodGet {
		return false
	}
	_, ok := participantRoutes[r.Method+" "+r.URL.Path]
	return ok
}
