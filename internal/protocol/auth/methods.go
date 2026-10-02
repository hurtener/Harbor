package auth

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"unicode"

	"github.com/hurtener/Harbor/internal/identity"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/methods"
)

const (
	// MethodReachClaim carries an optional, signed set of canonical methods.
	// Absence preserves existing authority; a present empty array denies all
	// methods. Membership never grants scopes, agent reach, or session reach.
	MethodReachClaim = "method_reach"
	// SessionAdmissionEpochClaim names the signed, positive admission epoch.
	SessionAdmissionEpochClaim = "session_admission_epoch"
	// SessionAdmissionCoordinatorClaim names the coordinator subject bound by
	// enrollment to the JWT issuer and the complete session identity.
	SessionAdmissionCoordinatorClaim = "session_admission_coordinator"
)

var (
	// ErrMethodReachMalformed rejects a present method claim that is not a
	// unique array of exact canonical method names.
	ErrMethodReachMalformed = errors.New("auth: method reach claim malformed")
	// ErrMethodReachDenied reports a method outside the signed restriction.
	ErrMethodReachDenied = errors.New("auth: method reach denied")
	// ErrSessionAdmissionMalformed rejects incomplete or malformed signed
	// admission authority. The epoch and coordinator are always a pair.
	ErrSessionAdmissionMalformed = errors.New("auth: session admission claim malformed")
)

type methodReachKey struct{}
type tokenAuthorityKey struct{}
type sessionAdmissionKey struct{}

// ParseMethodReach parses a PRESENT signed method_reach claim. Only names
// registered by methods.IsValidMethod are accepted, without alias expansion,
// trimming, case conversion, or wildcards. A signed null is not absence.
func ParseMethodReach(raw any) ([]methods.Method, error) {
	values, ok := raw.([]any)
	if !ok || values == nil {
		return nil, ErrMethodReachMalformed
	}
	if len(values) > len(methods.Methods()) {
		return nil, fmt.Errorf("%w: too many methods", ErrMethodReachMalformed)
	}
	reach := make([]methods.Method, 0, len(values))
	seen := make(map[methods.Method]struct{}, len(values))
	for _, value := range values {
		name, ok := value.(string)
		method := methods.Method(name)
		if !ok || !methods.IsValidMethod(method) {
			return nil, fmt.Errorf("%w: noncanonical method", ErrMethodReachMalformed)
		}
		if _, exists := seen[method]; exists {
			return nil, fmt.Errorf("%w: duplicate method", ErrMethodReachMalformed)
		}
		seen[method] = struct{}{}
		reach = append(reach, method)
	}
	return reach, nil
}

// WithMethodReach attaches a trusted restriction with a defensive copy.
// Calling it with nil explicitly restricts the context to no methods; omit
// the call entirely to represent an absent JWT claim.
func WithMethodReach(ctx context.Context, reach []methods.Method) context.Context {
	copyReach := make([]methods.Method, len(reach))
	copy(copyReach, reach)
	return context.WithValue(ctx, methodReachKey{}, copyReach)
}

// MethodReachFrom returns a defensive copy and distinguishes absent authority
// from a present empty restriction.
func MethodReachFrom(ctx context.Context) ([]methods.Method, bool) {
	reach, ok := ctx.Value(methodReachKey{}).([]methods.Method)
	if !ok {
		return nil, false
	}
	out := make([]methods.Method, len(reach))
	copy(out, reach)
	return out, true
}

// AuthorizeMethod enforces the optional signed restriction before canonical
// dispatch. Success leaves every existing scope, identity, session, and agent
// gate in force; even admin scope cannot widen method reach. Noncanonical
// method names always fail closed.
func AuthorizeMethod(ctx context.Context, method methods.Method) error {
	if !methods.IsValidMethod(method) {
		return ErrMethodReachDenied
	}
	reach, present := ctx.Value(methodReachKey{}).([]methods.Method)
	if !present {
		return nil
	}
	for _, allowed := range reach {
		if allowed == method {
			return nil
		}
	}
	return ErrMethodReachDenied
}

// TokenAuthority preserves the verified signing authority's issuer and subject
// for immutable enrollment binding. They are not isolation principals.
type TokenAuthority struct {
	Issuer  string
	Subject string
}

// WithTokenAuthority attaches issuer/subject from a verified token. This is a
// trusted in-process operation, never a projection of request body fields.
func WithTokenAuthority(ctx context.Context, authority TokenAuthority) context.Context {
	return context.WithValue(ctx, tokenAuthorityKey{}, authority)
}

// TokenAuthorityFrom returns the verified issuer/subject, if attached.
func TokenAuthorityFrom(ctx context.Context) (TokenAuthority, bool) {
	authority, ok := ctx.Value(tokenAuthorityKey{}).(TokenAuthority)
	return authority, ok
}

// SessionAdmissionAuthority carries signed authority for one identity triple.
// Identity is the original JWT identity, before any per-request session
// selection. The session mutation gate must match this entire identity,
// epoch, coordinator, and token issuer to the durable enrollment.
type SessionAdmissionAuthority struct {
	Epoch       uint64
	Coordinator string
	Identity    identity.Identity
}

// WithSessionAdmission attaches a trusted, value-copied authority. Nil masks
// any upstream admission authority; it never authorizes an admitted session.
func WithSessionAdmission(ctx context.Context, authority *SessionAdmissionAuthority) context.Context {
	if authority == nil {
		return context.WithValue(ctx, sessionAdmissionKey{}, (*SessionAdmissionAuthority)(nil))
	}
	return context.WithValue(ctx, sessionAdmissionKey{}, *authority)
}

// SessionAdmissionFrom returns a fresh value copy of verified authority.
func SessionAdmissionFrom(ctx context.Context) (*SessionAdmissionAuthority, bool) {
	authority, ok := ctx.Value(sessionAdmissionKey{}).(SessionAdmissionAuthority)
	if !ok {
		return nil, false
	}
	return &authority, true
}

// strictJWTClaims preserves exact JSON number spelling and rejects duplicate
// claim names before MapClaims can collapse conflicting signed authority.
// It does not establish trust: the ordinary JWT parser still verifies the
// signature, algorithm, issuer, audience, time bounds, and identity afterward.
func strictJWTClaims(rawToken string) (map[string]json.RawMessage, error) {
	parts := strings.Split(rawToken, ".")
	if len(parts) != 3 {
		return nil, ErrTokenMalformed
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, ErrTokenMalformed
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	first, err := decoder.Token()
	if err != nil || first != json.Delim('{') {
		return nil, ErrTokenMalformed
	}
	claims := make(map[string]json.RawMessage)
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return nil, ErrTokenMalformed
		}
		key, ok := keyToken.(string)
		if !ok {
			return nil, ErrTokenMalformed
		}
		if _, duplicate := claims[key]; duplicate {
			return nil, fmt.Errorf("%w: duplicate claim name", ErrTokenMalformed)
		}
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			return nil, ErrTokenMalformed
		}
		claims[key] = raw
	}
	if last, err := decoder.Token(); err != nil || last != json.Delim('}') {
		return nil, ErrTokenMalformed
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, ErrTokenMalformed
	}
	return claims, nil
}

func parseSessionAdmission(claims map[string]json.RawMessage, id identity.Identity, issuer string, methodReach []methods.Method) (*SessionAdmissionAuthority, error) {
	rawEpoch, hasEpoch := claims[SessionAdmissionEpochClaim]
	rawCoordinator, hasCoordinator := claims[SessionAdmissionCoordinatorClaim]
	if !hasEpoch && !hasCoordinator {
		return nil, nil
	}
	if !hasEpoch || !hasCoordinator || methodReach == nil {
		return nil, ErrSessionAdmissionMalformed
	}
	// JSON integers are decoded directly, never through float64: values above
	// 2^53 must retain their exact epoch, and fractions/exponents are rejected.
	epochText := strings.TrimSpace(string(rawEpoch))
	if epochText == "" || epochText[0] < '1' || epochText[0] > '9' {
		return nil, ErrSessionAdmissionMalformed
	}
	for _, char := range epochText {
		if char < '0' || char > '9' {
			return nil, ErrSessionAdmissionMalformed
		}
	}
	epoch, err := strconv.ParseUint(epochText, 10, 64)
	if err != nil {
		return nil, ErrSessionAdmissionMalformed
	}
	var coordinator string
	if err := json.Unmarshal(rawCoordinator, &coordinator); err != nil {
		return nil, ErrSessionAdmissionMalformed
	}
	if err := ValidateSessionAdmissionOwner(issuer, coordinator); err != nil {
		return nil, err
	}
	return &SessionAdmissionAuthority{Epoch: epoch, Coordinator: coordinator, Identity: id}, nil
}

// ValidateSessionAdmissionOwner checks the immutable issuer/coordinator pair
// using the same bounds as signed admission claims. Enrollment must call this
// before committing authority, so every enrolled owner can be represented by a
// valid future scoped token. Whitespace and control characters are forbidden.
func ValidateSessionAdmissionOwner(issuer, coordinator string) error {
	if !canonicalAuthorityName(issuer, 2048) || !canonicalAuthorityName(coordinator, 128) {
		return ErrSessionAdmissionMalformed
	}
	return nil
}

func canonicalAuthorityName(value string, limit int) bool {
	if value == "" || len(value) > limit {
		return false
	}
	for _, char := range value {
		if unicode.IsSpace(char) || unicode.IsControl(char) {
			return false
		}
	}
	return true
}

// exactAudience accepts JWT's scalar or singleton-array encoding only. A
// dual-audience token could pass a legacy verifier that ignores restrictions.
func exactAudience(raw any, expected string) bool {
	switch audience := raw.(type) {
	case string:
		return audience == expected
	case []any:
		if len(audience) != 1 {
			return false
		}
		value, ok := audience[0].(string)
		return ok && value == expected
	default:
		return false
	}
}

// MethodMiddleware applies signed method restrictions to HTTP routes before
// reading a body or invoking a handler. Unknown routes fail closed for a
// restricted bearer. Requests with no method restriction preserve existing
// routing behavior. Standalone mounts may compose this with trusted context
// establishment; Middleware already includes it after JWT verification.
func MethodMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, present := r.Context().Value(methodReachKey{}).([]methods.Method); !present {
			next.ServeHTTP(w, r)
			return
		}
		method, mapped := methodForRequest(r)
		if !mapped || AuthorizeMethod(r.Context(), method) != nil {
			writeProtocolError(w, http.StatusForbidden, protoerrors.Newf(protoerrors.CodeScopeMismatch,
				"method_reach: the requested method is not within this bearer's signed method reach"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

func methodForRequest(r *http.Request) (methods.Method, bool) {
	if r.Method == http.MethodGet && r.URL.Path == "/v1/events" {
		return methods.MethodEventsSubscribe, true
	}
	if r.Method != http.MethodPost {
		return "", false
	}
	if path, ok := strings.CutPrefix(r.URL.Path, "/v1/control/"); ok {
		method := methods.Method(path)
		if routed := r.PathValue("method"); routed != "" && routed != path {
			return "", false
		}
		return method, methods.IsValidMethod(method)
	}
	path, ok := strings.CutPrefix(r.URL.Path, "/v1/")
	if !ok || !strings.Contains(path, "/") || strings.Contains(path, ".") {
		return "", false
	}
	// Canonical direct routes render the method's dotted namespace as path
	// segments. No path cleaning, alias rewriting, or wildcard expansion is
	// permitted: only an exact registered canonical method can result.
	method := methods.Method(strings.ReplaceAll(path, "/", "."))
	return method, methods.IsValidMethod(method)
}
