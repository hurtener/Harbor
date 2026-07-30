package search_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/search"
)

const caller = "u-caller"

func reqWithUsers(users ...string) types.SearchRequest {
	return types.SearchRequest{Filter: types.SearchFilter{UserIDs: users}}
}

func reqWithTenants(tenants ...string) types.SearchRequest {
	return types.SearchRequest{Filter: types.SearchFilter{TenantIDs: tenants}}
}

// TestCrossUserRequested_Table pins the whole predicate. The `len>1`
// row matters most: a set naming the caller TWICE is still a fan-in, so
// the trigger is not "does it name somebody else" alone.
func TestCrossUserRequested_Table(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		users []string
		want  bool
	}{
		{"elided axis is not a crossing", nil, false},
		{"empty slice is not a crossing", []string{}, false},
		{"exactly the caller is not a crossing", []string{caller}, false},
		{"one foreign user is a crossing", []string{"u-victim"}, true},
		{"caller plus a foreign user is a crossing", []string{caller, "u-victim"}, true},
		{"the caller repeated is still a fan-in", []string{caller, caller}, true},
		{"two foreign users are a crossing", []string{"u-a", "u-b"}, true},
		{"a single empty string is a crossing", []string{""}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := search.CrossUserRequested(caller, reqWithUsers(tc.users...)); got != tc.want {
				t.Fatalf("CrossUserRequested(%q, %v) = %v, want %v", caller, tc.users, got, tc.want)
			}
		})
	}
}

// TestCrossUserRequested_MirrorsCrossTenantRequested drives BOTH axis
// predicates over one shared table, so a future edit to one that is not
// made to the other fails here rather than in production.
func TestCrossUserRequested_MirrorsCrossTenantRequested(t *testing.T) {
	t.Parallel()
	// The tenant predicate does NOT carry the len>1 fan-in trigger for a
	// repeated own value (it short-circuits on inequality only), so the
	// shared table covers the shapes where the two agree; the divergence
	// is asserted explicitly below it so it stays deliberate.
	shared := [][]string{nil, {}, {"own"}, {"foreign"}, {"own", "foreign"}, {"a", "b"}}
	for _, vals := range shared {
		gotUser := search.CrossUserRequested("own", reqWithUsers(vals...))
		gotTenant := search.CrossTenantRequested("own", reqWithTenants(vals...))
		if gotUser != gotTenant {
			t.Errorf("axis divergence on %v: user=%v tenant=%v", vals, gotUser, gotTenant)
		}
	}
	// The one deliberate divergence: the user axis treats a repeated own
	// value as a fan-in; the tenant axis (unchanged by this phase) does not.
	if !search.CrossUserRequested("own", reqWithUsers("own", "own")) {
		t.Error("CrossUserRequested: a repeated own user must read as a fan-in")
	}
	if search.CrossTenantRequested("own", reqWithTenants("own", "own")) {
		t.Error("CrossTenantRequested changed shape — reconcile the two predicates deliberately")
	}
}

// TestEffectiveUserSet_FoldsEmptyToCaller is the load-bearing assertion
// of the whole phase, asserted as a VALUE rather than as a row count: an
// elided user axis resolves to the caller's own user, never to a
// wildcard that storage reads as "every user in the tenant".
func TestEffectiveUserSet_FoldsEmptyToCaller(t *testing.T) {
	t.Parallel()
	for _, req := range []types.SearchRequest{{}, reqWithUsers(), reqWithUsers([]string{}...)} {
		got := search.EffectiveUserSet(caller, req)
		if !slices.Equal(got, []string{caller}) {
			t.Fatalf("EffectiveUserSet fold: got %v, want [%s]", got, caller)
		}
	}
}

func TestEffectiveUserSet_DeduplicatesAndSorts(t *testing.T) {
	t.Parallel()
	got := search.EffectiveUserSet(caller, reqWithUsers("u-c", "u-a", "u-c", "", "u-b"))
	if !slices.Equal(got, []string{"u-a", "u-b", "u-c"}) {
		t.Fatalf("EffectiveUserSet: got %v, want [u-a u-b u-c]", got)
	}
}

func TestEffectiveUserSet_AllEmptyFallsBackToCaller(t *testing.T) {
	t.Parallel()
	got := search.EffectiveUserSet(caller, reqWithUsers("", ""))
	if !slices.Equal(got, []string{caller}) {
		t.Fatalf("EffectiveUserSet all-empty: got %v, want [%s]", got, caller)
	}
}

// TestEffectiveUserSet_MirrorsEffectiveTenantSet drives the two
// effective-set helpers over ONE shared table. They are required to agree
// field for field; the assertion exists so an edit to one that is not
// made to the other fails.
func TestEffectiveUserSet_MirrorsEffectiveTenantSet(t *testing.T) {
	t.Parallel()
	table := [][]string{
		nil,
		{},
		{"own"},
		{"b", "a"},
		{"a", "a"},
		{"", "a"},
		{"", ""},
		{"z", "", "m", "z"},
	}
	for _, vals := range table {
		gotUser := search.EffectiveUserSet("own", reqWithUsers(vals...))
		gotTenant := search.EffectiveTenantSet("own", reqWithTenants(vals...))
		if !slices.Equal(gotUser, gotTenant) {
			t.Errorf("effective-set divergence on %v: user=%v tenant=%v", vals, gotUser, gotTenant)
		}
	}
}

// TestWidenedUserSet_DoesNotFoldElidedAxis pins the other half of the
// pair: a read an admin-tier claim authorised keeps its elided axis WIDE.
// Folding here would silently turn a fleet view into a self view.
func TestWidenedUserSet_DoesNotFoldElidedAxis(t *testing.T) {
	t.Parallel()
	if got := search.WidenedUserSet(types.SearchRequest{}); len(got) != 0 {
		t.Fatalf("WidenedUserSet on an elided axis: got %v, want the empty (every-user) set", got)
	}
	// On a NAMED axis the two helpers must agree exactly — the widened and
	// folded resolutions differ on one input only.
	named := reqWithUsers("u-b", "u-a", "u-b")
	if !slices.Equal(search.WidenedUserSet(named), search.EffectiveUserSet(caller, named)) {
		t.Errorf("WidenedUserSet and EffectiveUserSet disagree on a named axis: %v vs %v",
			search.WidenedUserSet(named), search.EffectiveUserSet(caller, named))
	}
}

func TestCrossSessionFanInRequested_OnlyMultiValueElevates(t *testing.T) {
	t.Parallel()
	cases := []struct {
		sessions []string
		want     bool
	}{
		{nil, false},
		{[]string{"s-own"}, false},
		// A single FOREIGN session is deliberately not a crossing: the
		// user fold above the session axis already decides whose rows are
		// in play, and gating it would break reading one's own sessions.
		{[]string{"s-somebody-elses"}, false},
		{[]string{"s-a", "s-b"}, true},
	}
	for _, tc := range cases {
		req := types.SearchRequest{Filter: types.SearchFilter{SessionIDs: tc.sessions}}
		if got := search.CrossSessionFanInRequested(req); got != tc.want {
			t.Errorf("CrossSessionFanInRequested(%v) = %v, want %v", tc.sessions, got, tc.want)
		}
	}
}

// TestQuery_CrossUserRefusedAtAggregateEdge — the refusal fires in Query
// BEFORE fan-out. `Query` rewrites the sub-request, so a per-index-only
// gate would be five chances to forget instead of one.
func TestQuery_CrossUserRefusedAtAggregateEdge(t *testing.T) {
	t.Parallel()
	s := &echoTenantSearcher{idx: types.SearchIndexSessions}
	reg, err := search.NewRegistry(s)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	id := identity.Identity{TenantID: "t1", UserID: caller, SessionID: "s1"}
	ctx, err := identity.With(context.Background(), id)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}

	cases := []struct {
		name string
		req  types.SearchRequest
		want error
	}{
		{"named foreign user", reqWithUsers("u-victim"), search.ErrCrossUserRequiresAdmin},
		{"multi-user fan-in", reqWithUsers(caller, "u-victim"), search.ErrCrossUserRequiresAdmin},
		{"own user repeated is still a fan-in", reqWithUsers(caller, caller), search.ErrCrossUserRequiresAdmin},
		{
			"multi-session fan-in",
			types.SearchRequest{Filter: types.SearchFilter{SessionIDs: []string{"s1", "s2"}}},
			search.ErrCrossSessionRequiresAdmin,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, qerr := search.Query(ctx, reg, id, denyAdmin, tc.req)
			if !errors.Is(qerr, tc.want) {
				t.Fatalf("Query: got %v, want %v", qerr, tc.want)
			}
		})
	}

	// The un-widened and the exactly-own-user shapes are indistinguishable
	// and neither needs a claim.
	for _, req := range []types.SearchRequest{{}, reqWithUsers(caller)} {
		if _, qerr := search.Query(ctx, reg, id, denyAdmin, req); qerr != nil {
			t.Fatalf("Query own-scope %v: %v", req.Filter.UserIDs, qerr)
		}
	}
	// A single own-other-session read still needs no claim.
	own := types.SearchRequest{Filter: types.SearchFilter{SessionIDs: []string{"s-my-other"}}}
	if _, qerr := search.Query(ctx, reg, id, denyAdmin, own); qerr != nil {
		t.Fatalf("Query own-other-session: %v", qerr)
	}
	// Under the claim, both widenings pass the edge.
	for _, req := range []types.SearchRequest{
		reqWithUsers("u-victim"),
		{Filter: types.SearchFilter{SessionIDs: []string{"s1", "s2"}}},
	} {
		if _, qerr := search.Query(ctx, reg, id, allowAdmin, req); qerr != nil {
			t.Fatalf("Query widened under claim: %v", qerr)
		}
	}
}

// TestQuery_PerIndexCrossUserErrorIsHard — a per-index cross-user refusal
// must PROPAGATE out of the aggregate merge rather than degrade into a
// partial union. The aggregate's graceful-degradation carve-out is for
// backend stutter, never for a scope refusal.
func TestQuery_PerIndexCrossUserErrorIsHard(t *testing.T) {
	t.Parallel()
	reg, err := search.NewRegistry(&errSearcher{
		idx: types.SearchIndexSessions,
		err: search.ErrCrossUserRequiresAdmin,
	})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	id := identity.Identity{TenantID: "t1", UserID: caller, SessionID: "s1"}
	ctx, _ := identity.With(context.Background(), id)
	// The edge gate passes (no filter), so the per-index error is the only
	// way the refusal can surface.
	_, qerr := search.Query(ctx, reg, id, denyAdmin, types.SearchRequest{})
	if !errors.Is(qerr, search.ErrCrossUserRequiresAdmin) {
		t.Fatalf("Query: got %v, want the cross-user refusal to propagate", qerr)
	}
}

// errSearcher always fails with a fixed error — used to prove the
// aggregate's hard-error set.
type errSearcher struct {
	idx types.SearchIndex
	err error
}

func (e *errSearcher) Index() types.SearchIndex { return e.idx }

func (e *errSearcher) Search(context.Context, types.SearchRequest) (types.SearchResponse, error) {
	return types.SearchResponse{}, e.err
}
