package artifacts_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	artifactsubsys "github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	eventsubsys "github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	protocolauth "github.com/hurtener/Harbor/internal/protocol/auth"
	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/search"
	artifactsearch "github.com/hurtener/Harbor/internal/search/artifacts"
	"github.com/hurtener/Harbor/internal/server"
)

// Two users in ONE tenant. The store's `List` precondition requires only
// the TENANT and documents an empty `UserID` as a wildcard — correct at
// the store, since a list filter is a predicate over a result set rather
// than an identity. What an omitted component MEANS is this searcher's
// call, and it never made it.
const (
	oneTenant = "t1"
	attacker  = "attacker"
	victim    = "victim"
)

func denyingSearcher(t *testing.T, store artifactsubsys.ArtifactStore) *artifactsearch.Searcher {
	t.Helper()
	s, err := artifactsearch.New(store, search.Deps{
		Redactor:   patterns.New(),
		AdminScope: func(context.Context) bool { return false },
		Audit:      testAudit,
	})
	if err != nil {
		t.Fatalf("artifactsearch.New: %v", err)
	}
	return s
}

func claimedSearcher(t *testing.T, store artifactsubsys.ArtifactStore) *artifactsearch.Searcher {
	t.Helper()
	s, err := artifactsearch.New(store, search.Deps{
		Redactor:   patterns.New(),
		AdminScope: server.SearchAdminScopeFromAuth,
		Audit:      testAudit,
	})
	if err != nil {
		t.Fatalf("artifactsearch.New: %v", err)
	}
	return s
}

func seedTwoUsersOneTenant(t *testing.T, store artifactsubsys.ArtifactStore) {
	t.Helper()
	for _, u := range []string{attacker, victim} {
		putArtifact(t, store, artifactsubsys.ArtifactScope{
			TenantID: oneTenant, UserID: u, SessionID: u + "-sess",
		}, u+"-file", artifactsubsys.PutOpts{Filename: u + "-secret.txt", MimeType: "text/plain"})
	}
}

func attackerCtx() context.Context {
	ctx, _ := identity.With(context.Background(), identity.Identity{
		TenantID: oneTenant, UserID: attacker, SessionID: attacker + "-sess",
	})
	return ctx
}

// TestArtifactsSearcher_ElidedUserFoldsToCaller — the elision arm. Before
// the fold this searcher built `ArtifactScope{TenantID: tenant}` with an
// unset `UserID`, so the DEFAULT request enumerated every user's artifact
// catalog in the tenant, filenames and content digests included.
func TestArtifactsSearcher_ElidedUserFoldsToCaller(t *testing.T) {
	t.Parallel()
	store := newStore(t)
	defer store.Close(context.Background())
	seedTwoUsersOneTenant(t, store)

	resp, err := denyingSearcher(t, store).Search(attackerCtx(), types.SearchRequest{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(resp.Rows) == 0 {
		t.Fatal("the fold returned nothing — a fix that empties a working surface is not a fix")
	}
	for _, r := range resp.Rows {
		if r.UserID != attacker {
			t.Errorf("CROSS-USER LEAK: artifact row %s belongs to %q", r.ID, r.UserID)
		}
		if r.Ref != nil && strings.HasPrefix(r.Ref.Filename, victim) {
			t.Errorf("CROSS-USER LEAK in the Ref: %q", r.Ref.Filename)
		}
	}
}

func TestArtifactsSearcher_NamedForeignUserRefused(t *testing.T) {
	t.Parallel()
	store := newStore(t)
	defer store.Close(context.Background())
	seedTwoUsersOneTenant(t, store)

	_, err := denyingSearcher(t, store).Search(attackerCtx(), types.SearchRequest{
		Filter: types.SearchFilter{UserIDs: []string{victim}},
	})
	if !errors.Is(err, search.ErrCrossUserRequiresAdmin) {
		t.Fatalf("named foreign user: got %v, want ErrCrossUserRequiresAdmin", err)
	}
}

func TestArtifactsSearcher_GrantedWideningsEmitCanonicalAuditBeforeRead(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		req  types.SearchRequest
	}{
		{name: "tenant axis", req: types.SearchRequest{Filter: types.SearchFilter{
			TenantIDs: []string{"tenant-target"}, UserIDs: []string{attacker},
		}}},
		{name: "user axis", req: types.SearchRequest{Filter: types.SearchFilter{UserIDs: []string{victim}}}},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store := newStore(t)
			defer store.Close(context.Background())
			seedTwoUsersOneTenant(t, store)
			var got []eventsubsys.Event
			s, err := artifactsearch.New(store, search.Deps{
				Redactor: patterns.New(), AdminScope: server.SearchAdminScopeFromAuth,
				Audit: func(_ context.Context, ev eventsubsys.Event) error {
					got = append(got, ev)
					return nil
				},
			})
			if err != nil {
				t.Fatalf("artifactsearch.New: %v", err)
			}
			ctx := protocolauth.WithScopes(attackerCtx(), []protocolauth.Scope{protocolauth.ScopeAdmin})
			if _, err := s.Search(ctx, tc.req); err != nil {
				t.Fatalf("Search: %v", err)
			}
			if len(got) != 1 || got[0].Type != eventsubsys.EventTypeAdminScopeUsed {
				t.Fatalf("audit events = %+v, want one audit.admin_scope_used", got)
			}
		})
	}
}

func TestArtifactsSearcher_OwnUserNamedNeedsNoClaim(t *testing.T) {
	t.Parallel()
	store := newStore(t)
	defer store.Close(context.Background())
	seedTwoUsersOneTenant(t, store)

	s := denyingSearcher(t, store)
	elided, err := s.Search(attackerCtx(), types.SearchRequest{})
	if err != nil {
		t.Fatalf("elided: %v", err)
	}
	named, err := s.Search(attackerCtx(), types.SearchRequest{
		Filter: types.SearchFilter{UserIDs: []string{attacker}},
	})
	if err != nil {
		t.Fatalf("own user named: %v", err)
	}
	if len(named.Rows) != len(elided.Rows) || len(named.Rows) == 0 {
		t.Fatalf("own-user-named must equal elided: named=%d elided=%d", len(named.Rows), len(elided.Rows))
	}
}

func TestArtifactsSearcher_MultiUserFanInRefused(t *testing.T) {
	t.Parallel()
	store := newStore(t)
	defer store.Close(context.Background())
	seedTwoUsersOneTenant(t, store)

	s := denyingSearcher(t, store)
	for _, users := range [][]string{{attacker, victim}, {attacker, attacker}} {
		_, err := s.Search(attackerCtx(), types.SearchRequest{
			Filter: types.SearchFilter{UserIDs: users},
		})
		if !errors.Is(err, search.ErrCrossUserRequiresAdmin) {
			t.Errorf("multi-user %v: got %v, want ErrCrossUserRequiresAdmin", users, err)
		}
	}
}

func TestArtifactsSearcher_AdminClaimReopensBothWidenings(t *testing.T) {
	t.Parallel()
	for _, scope := range []protocolauth.Scope{protocolauth.ScopeAdmin, protocolauth.ScopeConsoleFleet} {
		t.Run(string(scope), func(t *testing.T) {
			t.Parallel()
			store := newStore(t)
			defer store.Close(context.Background())
			seedTwoUsersOneTenant(t, store)

			s := claimedSearcher(t, store)
			ctx := protocolauth.WithScopes(attackerCtx(), []protocolauth.Scope{scope})

			named, err := s.Search(ctx, types.SearchRequest{
				Filter: types.SearchFilter{UserIDs: []string{victim}},
			})
			if err != nil {
				t.Fatalf("named foreign user under %s: %v", scope, err)
			}
			if len(named.Rows) != 1 || named.Rows[0].UserID != victim {
				t.Fatalf("named foreign user under %s: got %d rows, want 1 owned by %s",
					scope, len(named.Rows), victim)
			}

			// The elided axis of a WIDENED read is the one case where the
			// store's every-user wildcard is the right answer.
			elided, err := s.Search(ctx, types.SearchRequest{})
			if err != nil {
				t.Fatalf("elided under %s: %v", scope, err)
			}
			if len(elided.Rows) != 2 {
				t.Fatalf("a widened read must NOT fold its elided user axis: got %d rows, want 2", len(elided.Rows))
			}
		})
	}
}

// TestArtifactsSearcher_IteratesEffectiveUserSet — the loop-shape
// regression pin. Reading only `UserIDs[0]` silently dropped users 2..N,
// so a widened two-user read answered one user's rows and looked like a
// working fan-in.
func TestArtifactsSearcher_IteratesEffectiveUserSet(t *testing.T) {
	t.Parallel()
	store := newStore(t)
	defer store.Close(context.Background())
	seedTwoUsersOneTenant(t, store)

	ctx := protocolauth.WithScopes(attackerCtx(), []protocolauth.Scope{protocolauth.ScopeAdmin})
	resp, err := claimedSearcher(t, store).Search(ctx, types.SearchRequest{
		Filter: types.SearchFilter{UserIDs: []string{attacker, victim}},
	})
	if err != nil {
		t.Fatalf("widened two-user read: %v", err)
	}
	seen := map[string]bool{}
	for _, r := range resp.Rows {
		seen[r.UserID] = true
	}
	if !seen[attacker] || !seen[victim] {
		t.Fatalf("a widened two-user read returned %v — every named user must be iterated", seen)
	}
}

// TestArtifactsSearcher_HeavyPreviewBindsFlag — the §17.6 sibling fix.
// Asserted through the ROW SHAPE rather than through the discard: a
// preview at or above the bound ships an empty Preview beside the Ref
// this index always populates, and that is now true by construction
// rather than by coincidence.
func TestArtifactsSearcher_HeavyPreviewBindsFlag(t *testing.T) {
	t.Parallel()
	store := newStore(t)
	defer store.Close(context.Background())

	// The preview is built from the artifact's own metadata, so a filename
	// past the bound is the only way to reach the heavy arm.
	huge := strings.Repeat("h", search.HeavyPreviewThreshold+1) + ".bin"
	putArtifact(t, store, artifactsubsys.ArtifactScope{
		TenantID: oneTenant, UserID: attacker, SessionID: attacker + "-sess",
	}, "huge", artifactsubsys.PutOpts{Filename: huge, MimeType: "application/octet-stream"})

	resp, err := denyingSearcher(t, store).Search(attackerCtx(), types.SearchRequest{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(resp.Rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(resp.Rows))
	}
	row := resp.Rows[0]
	if row.Preview != "" {
		t.Errorf("a preview past the bound must not ship inline: got %d bytes", len(row.Preview))
	}
	if row.Ref == nil {
		t.Fatal("every artifact row carries a Ref — the heavy arm decides the PREVIEW, never addressability")
	}
	if row.Ref.Filename != huge {
		t.Errorf("Ref.Filename = %q, want the stored filename", row.Ref.Filename)
	}

	// And the ordinary arm still ships a preview beside the same Ref.
	putArtifact(t, store, artifactsubsys.ArtifactScope{
		TenantID: oneTenant, UserID: attacker, SessionID: attacker + "-sess",
	}, "small", artifactsubsys.PutOpts{Filename: "small.txt", MimeType: "text/plain"})
	resp, err = denyingSearcher(t, store).Search(attackerCtx(), types.SearchRequest{Query: "small.txt"})
	if err != nil {
		t.Fatalf("Search small: %v", err)
	}
	if len(resp.Rows) != 1 || resp.Rows[0].Preview == "" || resp.Rows[0].Ref == nil {
		t.Fatalf("an under-bound row must carry BOTH a preview and a Ref: %+v", resp.Rows)
	}
}
