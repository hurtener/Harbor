package postgres

import (
	"context"
	"regexp"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/skills"
)

func TestBuildRegex_FailsClosedAndTokenizesNaturalLanguage(t *testing.T) {
	t.Parallel()

	for _, query := range []string{"", "   ", "["} {
		if _, err := buildRegex(query); err == nil {
			t.Errorf("buildRegex(%q) returned nil error", query)
		}
	}

	re, err := buildRegex("Harbor planner")
	if err != nil {
		t.Fatalf("buildRegex(natural language): %v", err)
	}
	for _, value := range []string{"harbor", "PLANNER"} {
		if !re.MatchString(value) {
			t.Errorf("tokenized regex did not match %q", value)
		}
	}
}

func TestRegexScore_UsesDocumentedFieldPrecedence(t *testing.T) {
	t.Parallel()

	skill := skills.Skill{
		Name:        "alpha",
		Title:       "Harbor Title",
		Description: "planner body",
		Trigger:     "on request",
		Tags:        []string{"durable"},
	}
	tests := []struct {
		name  string
		query string
		want  float64
	}{
		{name: "full name", query: "alpha", want: 0.95},
		{name: "name prefix", query: "alph", want: 0.90},
		{name: "name substring", query: "lph", want: 0.85},
		{name: "body", query: "planner", want: 0.75},
		{name: "tag", query: "durable", want: 0.75},
		{name: "miss", query: "absent", want: 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			re := regexp.MustCompile("(?i)" + tc.query)
			if got := regexScore(re, skill); got != tc.want {
				t.Fatalf("regexScore(%q) = %v, want %v", tc.query, got, tc.want)
			}
		})
	}
}

func TestSnapshotFTSResults_ValidatesOrdinalsAndNormalizesScores(t *testing.T) {
	t.Parallel()

	candidates := []skills.Skill{
		{Name: "older", UpdatedAt: time.Unix(1, 0).UTC()},
		{Name: "newer", UpdatedAt: time.Unix(2, 0).UTC()},
	}
	if got, err := snapshotFTSResults(candidates, nil); err != nil || got != nil {
		t.Fatalf("empty hits = (%v, %v), want (nil, nil)", got, err)
	}
	for _, ordinal := range []int{-1, len(candidates)} {
		if _, err := snapshotFTSResults(candidates, []snapshotFTSHit{{ordinal: ordinal, raw: 1}}); err == nil {
			t.Errorf("out-of-range ordinal %d returned nil error", ordinal)
		}
	}

	got, err := snapshotFTSResults(candidates, []snapshotFTSHit{
		{ordinal: 0, raw: 2},
		{ordinal: 1, raw: 5},
	})
	if err != nil {
		t.Fatalf("snapshotFTSResults: %v", err)
	}
	if len(got) != 2 || got[0].Skill.Name != "newer" || got[0].Score != 1 || got[1].Score != 0 {
		t.Fatalf("normalized results = %+v", got)
	}

	equal, err := snapshotFTSResults(candidates, []snapshotFTSHit{
		{ordinal: 0, raw: 3},
		{ordinal: 1, raw: 3},
	})
	if err != nil {
		t.Fatalf("snapshotFTSResults(equal): %v", err)
	}
	if equal[0].Score != 1 || equal[1].Score != 1 || equal[0].Skill.Name != "newer" {
		t.Fatalf("equal-rank results = %+v", equal)
	}
}

func TestJSONHelpers_RoundTripAndRejectUnsupportedValues(t *testing.T) {
	t.Parallel()

	encoded, err := marshalStrings([]string{"alpha", "beta"})
	if err != nil {
		t.Fatalf("marshalStrings: %v", err)
	}
	decoded := unmarshalStrings(encoded)
	if len(decoded) != 2 || decoded[0] != "alpha" || decoded[1] != "beta" {
		t.Fatalf("unmarshalStrings(%q) = %v", encoded, decoded)
	}
	if got := unmarshalStrings("not-json"); got != nil {
		t.Fatalf("malformed string JSON = %v, want nil", got)
	}

	extra := map[string]any{"enabled": true, "count": float64(2)}
	extraJSON, err := marshalExtra(extra)
	if err != nil {
		t.Fatalf("marshalExtra: %v", err)
	}
	gotExtra := unmarshalExtra(extraJSON)
	if gotExtra["enabled"] != true || gotExtra["count"] != float64(2) {
		t.Fatalf("unmarshalExtra(%q) = %#v", extraJSON, gotExtra)
	}
	if got := unmarshalExtra("not-json"); got != nil {
		t.Fatalf("malformed extra JSON = %#v, want nil", got)
	}
	if _, err := marshalExtra(map[string]any{"unsupported": make(chan int)}); err == nil {
		t.Fatal("marshalExtra accepted an unsupported value")
	}
}

func TestEmbedIdentityCtx_SeatsRunAndAuditsTenantCrossing(t *testing.T) {
	t.Parallel()

	id := identity.Quadruple{
		Identity: identity.Identity{TenantID: "tenant", UserID: "user", SessionID: "session"},
		RunID:    "run",
	}
	ctx, err := embedIdentityCtx(context.Background(), id)
	if err != nil {
		t.Fatalf("embedIdentityCtx: %v", err)
	}
	got, ok := identity.QuadrupleFrom(ctx)
	if !ok || got != id {
		t.Fatalf("quadruple = (%+v, %v), want %+v", got, ok, id)
	}

	verifiedID := identity.Identity{
		TenantID: "other-tenant", UserID: "user", SessionID: "session",
	}
	verified, err := identity.WithVerified(context.Background(), verifiedID)
	if err != nil {
		t.Fatalf("identity.WithVerified: %v", err)
	}
	crossed, err := embedIdentityCtx(verified, id)
	if err != nil {
		t.Fatalf("embedIdentityCtx(cross-tenant): %v", err)
	}
	if tenant, ok := identity.ElevatedTenant(crossed); !ok || tenant != id.TenantID {
		t.Fatalf("elevated tenant = (%q, %v), want %q", tenant, ok, id.TenantID)
	}
	if got, ok := identity.QuadrupleFrom(crossed); !ok || got != id {
		t.Fatalf("crossed quadruple = (%+v, %v), want %+v", got, ok, id)
	}
	if got, ok := identity.FromVerified(crossed); !ok || got != verifiedID {
		t.Fatalf("verified anchor = (%+v, %v), want %+v", got, ok, verifiedID)
	}

	withoutRun := id
	withoutRun.RunID = ""
	working, err := embedIdentityCtx(context.Background(), withoutRun)
	if err != nil {
		t.Fatalf("embedIdentityCtx(no run): %v", err)
	}
	if got, ok := identity.From(working); !ok || got != id.Identity {
		t.Fatalf("working identity = (%+v, %v), want %+v", got, ok, id.Identity)
	}
	crossedWithoutRun, err := embedIdentityCtx(verified, withoutRun)
	if err != nil {
		t.Fatalf("embedIdentityCtx(cross-tenant, no run): %v", err)
	}
	if tenant, ok := identity.ElevatedTenant(crossedWithoutRun); !ok || tenant != id.TenantID {
		t.Fatalf("no-run elevated tenant = (%q, %v), want %q", tenant, ok, id.TenantID)
	}
}
