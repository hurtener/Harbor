package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/state"
	"github.com/hurtener/Harbor/internal/tools"
)

// TokenStore Kind shapes. The store persists through the generic
// state.StateStore (D-027): one document per
// (tenant, scope, subject_id, source). Composite-key encoding lives
// in tokenKind so the StateStore stays generic.
const (
	// tokenKindPrefix is the "tools.auth.token." namespace for the
	// underlying StateStore record. The full Kind is
	// "tools.auth.token.<scope>.<subject_id>.<source>".
	tokenKindPrefix = "tools.auth.token." //nolint:gosec // G101 false positive: this is a StateStore Kind namespace prefix, not a credential

	// accessTokenKindPrefix is the "tools.auth.access." namespace for
	// the access-token-only record. Access and refresh tokens are
	// persisted under separate Kind prefixes so a (post-V1) caller
	// that wants to read access-token TTL without unsealing the
	// refresh-token half can — and so a compromise of the
	// access-token cache does not yield refresh capability (brief 09
	// §"Encryption at rest").
	accessTokenKindPrefix = "tools.auth.access." //nolint:gosec // G101 false positive: this is a StateStore Kind namespace prefix, not a credential

	// refreshTokenKindPrefix mirrors accessTokenKindPrefix.
	refreshTokenKindPrefix = "tools.auth.refresh." //nolint:gosec // G101 false positive: this is a StateStore Kind namespace prefix, not a credential
)

// tokenKind builds the StateStore Kind for the access-token half of a
// token's persistence pair.
func tokenKind(scope BindingScope, subjectID string, source tools.ToolSourceID) string {
	return accessTokenKindPrefix + string(scope) + "." + subjectID + "." + string(source)
}

// refreshKind builds the StateStore Kind for the refresh-token half.
func refreshKind(scope BindingScope, subjectID string, source tools.ToolSourceID) string {
	return refreshTokenKindPrefix + string(scope) + "." + subjectID + "." + string(source)
}

// stateStoreTokenStore is the V1 TokenStore implementation — a typed
// wrapper around the §4.4 state.StateStore seam (D-027 + D-067 +
// D-068 pattern). Driver pluralism (in-mem / SQLite / Postgres) lives
// at the StateStore layer; the TokenStore is single-concrete.
//
// Concurrent reuse (D-025): the StateStore is itself concurrent-safe
// per D-027; the Sealer is concurrent-safe per crypto/cipher; the
// store struct holds only immutable references. Per-call state lives
// in ctx + arguments.
type stateStoreTokenStore struct {
	store  state.StateStore
	sealer Sealer
	now    func() time.Time
}

// NewTokenStore constructs a TokenStore over the given StateStore +
// Sealer. Both are mandatory — a nil store / nil sealer is rejected
// at construction (fail-loud per CLAUDE.md §13 amendment).
func NewTokenStore(store state.StateStore, sealer Sealer) (TokenStore, error) {
	if store == nil {
		return nil, errors.New("auth: NewTokenStore: state.StateStore required")
	}
	if sealer == nil {
		return nil, errors.New("auth: NewTokenStore: Sealer required (encryption-at-rest is mandatory)")
	}
	return &stateStoreTokenStore{
		store:  store,
		sealer: sealer,
		now:    time.Now,
	}, nil
}

// tokenEnvelope is the JSON-shaped record stored in the StateStore.
// AccessTokenCipher / RefreshTokenCipher hold the AES-GCM-sealed
// blobs; the rest is plaintext metadata safe for at-rest exposure
// (TenantID / UserID / AgentID are the identity scope the record
// belongs to — not secret).
type tokenEnvelope struct {
	Source             string    `json:"source"`
	BindingScope       string    `json:"binding_scope"`
	TenantID           string    `json:"tenant_id"`
	UserID             string    `json:"user_id,omitempty"`
	AgentID            string    `json:"agent_id,omitempty"`
	AccessTokenCipher  []byte    `json:"access_token_cipher"`
	RefreshTokenCipher []byte    `json:"refresh_token_cipher,omitempty"`
	TokenType          string    `json:"token_type,omitempty"`
	ExpiresAt          time.Time `json:"expires_at,omitempty"`
	Scopes             []string  `json:"scopes,omitempty"`
	LastRefreshedAt    time.Time `json:"last_refreshed_at,omitempty"`
}

// Get returns the Token at (ctx identity, scope, subject, source).
// Returns (Token{}, false, nil) on miss; (Token{}, false, err) on
// store or cipher failure.
func (s *stateStoreTokenStore) Get(ctx context.Context, scope BindingScope, subjectID string, source tools.ToolSourceID) (Token, bool, error) {
	if err := ctx.Err(); err != nil {
		return Token{}, false, fmt.Errorf("auth: Get cancelled: %w", err)
	}
	id, err := identityFromCtx(ctx)
	if err != nil {
		return Token{}, false, err
	}
	if !IsValidBindingScope(scope) {
		return Token{}, false, wrap(ErrInvalidBindingScope, "got %q", string(scope))
	}
	if subjectID == "" {
		return Token{}, false, wrap(ErrConfigInvalid, "subjectID empty (scope=%s)", scope)
	}
	if source == "" {
		return Token{}, false, wrap(ErrConfigInvalid, "source empty")
	}

	q := identity.Quadruple{Identity: id}
	rec, err := s.store.Load(ctx, q, tokenKind(scope, subjectID, source))
	if err != nil {
		if errors.Is(err, state.ErrNotFound) {
			return Token{}, false, nil
		}
		return Token{}, false, fmt.Errorf("auth: Get load: %w", err)
	}

	var env tokenEnvelope
	if err := json.Unmarshal(rec.Bytes, &env); err != nil {
		return Token{}, false, fmt.Errorf("%w: envelope decode: %w",
			ErrTokenCipherCorrupt, err)
	}

	access, err := s.sealer.Open(env.AccessTokenCipher)
	if err != nil {
		return Token{}, false, err
	}
	var refresh []byte
	if len(env.RefreshTokenCipher) > 0 {
		refresh, err = s.sealer.Open(env.RefreshTokenCipher)
		if err != nil {
			return Token{}, false, err
		}
	}

	tok := Token{
		Source:          tools.ToolSourceID(env.Source),
		BindingScope:    BindingScope(env.BindingScope),
		TenantID:        env.TenantID,
		UserID:          env.UserID,
		AgentID:         env.AgentID,
		AccessToken:     string(access),
		RefreshToken:    string(refresh),
		TokenType:       env.TokenType,
		ExpiresAt:       env.ExpiresAt,
		Scopes:          append([]string(nil), env.Scopes...),
		LastRefreshedAt: env.LastRefreshedAt,
	}
	return tok, true, nil
}

// Put encrypts the access + refresh tokens and persists. Identity is
// mandatory; the calling ctx identity gates which scope the record
// lives in. The Token's TenantID / UserID / AgentID fields are read
// from t — the caller is responsible for setting them, since
// agent-bound tokens may be persisted under an admin's ctx but key on
// the agent_id in t (brief 09).
func (s *stateStoreTokenStore) Put(ctx context.Context, t Token) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("auth: Put cancelled: %w", err)
	}
	id, err := identityFromCtx(ctx)
	if err != nil {
		return err
	}
	if !IsValidBindingScope(t.BindingScope) {
		return wrap(ErrInvalidBindingScope, "got %q", string(t.BindingScope))
	}
	if t.Source == "" {
		return wrap(ErrConfigInvalid, "source empty")
	}
	subj := t.SubjectID()
	if subj == "" {
		return wrap(ErrConfigInvalid, "subject empty for scope %s", t.BindingScope)
	}
	if t.AccessToken == "" {
		return wrap(ErrConfigInvalid, "AccessToken empty")
	}
	if t.TenantID == "" {
		// Default to ctx tenant if caller did not set; defensive but
		// the audit trail is clearer when caller sets explicitly.
		t.TenantID = id.TenantID
	}
	if t.TenantID != id.TenantID {
		return wrap(ErrIdentityRequired, "token tenant %q != ctx tenant %q", t.TenantID, id.TenantID)
	}

	accessCipher, err := s.sealer.Seal([]byte(t.AccessToken))
	if err != nil {
		return fmt.Errorf("auth: seal access: %w", err)
	}
	var refreshCipher []byte
	if t.RefreshToken != "" {
		refreshCipher, err = s.sealer.Seal([]byte(t.RefreshToken))
		if err != nil {
			return fmt.Errorf("auth: seal refresh: %w", err)
		}
	}

	env := tokenEnvelope{
		Source:             string(t.Source),
		BindingScope:       string(t.BindingScope),
		TenantID:           t.TenantID,
		UserID:             t.UserID,
		AgentID:            t.AgentID,
		AccessTokenCipher:  accessCipher,
		RefreshTokenCipher: refreshCipher,
		TokenType:          t.TokenType,
		ExpiresAt:          t.ExpiresAt,
		Scopes:             append([]string(nil), t.Scopes...),
		LastRefreshedAt:    t.LastRefreshedAt,
	}
	bytes, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("auth: marshal envelope: %w", err)
	}

	q := identity.Quadruple{Identity: id}
	rec := state.StateRecord{
		ID:        state.NewEventID(),
		Identity:  q,
		Kind:      tokenKind(t.BindingScope, subj, t.Source),
		Bytes:     bytes,
		UpdatedAt: s.now(),
	}
	if err := s.store.Save(ctx, rec); err != nil {
		return fmt.Errorf("auth: Put save: %w", err)
	}

	// Refresh-token sibling record under a parallel Kind so a
	// post-V1 reader that only needs the access-token TTL does not
	// pay the refresh-decode cost. Same composite-key suffix; the
	// Bytes are the AES-GCM-sealed refresh-token plaintext (no
	// envelope around it — the access-token envelope already records
	// the metadata).
	if len(refreshCipher) > 0 {
		refreshRec := state.StateRecord{
			ID:        state.NewEventID(),
			Identity:  q,
			Kind:      refreshKind(t.BindingScope, subj, t.Source),
			Bytes:     refreshCipher,
			UpdatedAt: s.now(),
		}
		if err := s.store.Save(ctx, refreshRec); err != nil {
			return fmt.Errorf("auth: Put save (refresh sibling): %w", err)
		}
	}

	return nil
}

// Delete removes the access + refresh records for
// (ctx identity, scope, subject, source). Idempotent — missing
// records are not an error.
func (s *stateStoreTokenStore) Delete(ctx context.Context, scope BindingScope, subjectID string, source tools.ToolSourceID) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("auth: Delete cancelled: %w", err)
	}
	id, err := identityFromCtx(ctx)
	if err != nil {
		return err
	}
	if !IsValidBindingScope(scope) {
		return wrap(ErrInvalidBindingScope, "got %q", string(scope))
	}
	if subjectID == "" {
		return wrap(ErrConfigInvalid, "subjectID empty")
	}
	if source == "" {
		return wrap(ErrConfigInvalid, "source empty")
	}

	q := identity.Quadruple{Identity: id}
	if err := s.store.Delete(ctx, q, tokenKind(scope, subjectID, source)); err != nil {
		return fmt.Errorf("auth: Delete access: %w", err)
	}
	if err := s.store.Delete(ctx, q, refreshKind(scope, subjectID, source)); err != nil {
		return fmt.Errorf("auth: Delete refresh: %w", err)
	}
	return nil
}

// identityFromCtx pulls the Identity triple from ctx and fails closed
// when any component is missing.
func identityFromCtx(ctx context.Context) (identity.Identity, error) {
	id, ok := identity.From(ctx)
	if !ok {
		return identity.Identity{}, ErrIdentityRequired
	}
	if err := identity.Validate(id); err != nil {
		return identity.Identity{}, fmt.Errorf("%w: %w", ErrIdentityRequired, err)
	}
	return id, nil
}

// kindBelongsToToken reports whether kind is one of the token Kinds
// this package owns. Used by audit / debug paths to disambiguate
// token records from other StateStore records.
func kindBelongsToToken(kind string) bool {
	return strings.HasPrefix(kind, tokenKindPrefix) ||
		strings.HasPrefix(kind, accessTokenKindPrefix) ||
		strings.HasPrefix(kind, refreshTokenKindPrefix)
}
