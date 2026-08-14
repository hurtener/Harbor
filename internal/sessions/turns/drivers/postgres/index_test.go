package postgres_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/sessions/turns"
	"github.com/hurtener/Harbor/internal/sessions/turns/drivers/postgres"
)

// TestPostgres_Indexes_QueriesUseIndexScans pins the INDEXED path of
// the store contract with EXPLAIN: indexed get (primary key), stable
// keyset paging (turn_rows_keyset), the exact older-row count
// (turn_rows_keyset), retention eviction ordering (turn_rows_keyset),
// and the identity + effective-agent + session/root-turn lookup
// (turn_rows_by_agent) must all resolve through their index — never a
// sequential scan, never an OFFSET / history scan. DSN-dependent;
// these assertions only mean something against a real planner.
func TestPostgres_Indexes_QueriesUseIndexScans(t *testing.T) {
	baseDSN := requireDSN(t)
	dsn := freshSchema(t, baseDSN)

	s, err := postgres.New(postgres.Config{DSN: dsn})
	if err != nil {
		t.Fatalf("postgres.New: %v", err)
	}
	defer func() { _ = s.Close(context.Background()) }()
	ctx := context.Background()

	// Seed enough rows for the planner to prefer the indexes, then
	// ANALYZE so the planner's cost estimates are real.
	const n = 5000
	for i := range n {
		row := freshRow(fmt.Sprintf("run-%05d", i))
		row.Agent = turns.Agent{ID: "agent-main", Complete: turns.CompletenessComplete}
		if _, err := s.AppendTurnIf(ctx, fixtureID, row); err != nil {
			t.Fatalf("seed append %d: %v", i, err)
		}
	}
	other := identity.Identity{TenantID: "t-other", UserID: "u-other", SessionID: "s-other"}
	if _, err := s.AppendTurnIf(ctx, other, freshRow("run-00000")); err != nil {
		t.Fatalf("seed other session: %v", err)
	}

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("explain sql.Open: %v", err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.ExecContext(ctx, "ANALYZE turn_rows"); err != nil {
		t.Fatalf("ANALYZE: %v", err)
	}

	// The keyset boundary used by the page + count queries below.
	var boundarySeq int64
	if err := db.QueryRowContext(ctx, `
        SELECT sequence FROM turn_rows
        WHERE tenant_id = $1 AND user_id = $2 AND session_id = $3 AND turn_id = $4`,
		fixtureID.TenantID, fixtureID.UserID, fixtureID.SessionID, "run-01000",
	).Scan(&boundarySeq); err != nil {
		t.Fatalf("boundary lookup: %v", err)
	}

	keyset := fmt.Sprintf(`(sequence < %d OR (sequence = %d AND turn_id < 'run-01000'))`,
		boundarySeq, boundarySeq)
	cases := []struct {
		name  string
		query string
		want  string // the index the plan must resolve through
	}{
		{
			name: "indexed_get_primary_key",
			query: fmt.Sprintf(`SELECT row_json FROM turn_rows
                WHERE tenant_id = '%s' AND user_id = '%s' AND session_id = '%s' AND turn_id = 'run-00001'`,
				fixtureID.TenantID, fixtureID.UserID, fixtureID.SessionID),
			want: "turn_rows_pkey",
		},
		{
			name: "keyset_page_newest_first",
			query: fmt.Sprintf(`SELECT row_json FROM turn_rows
                WHERE tenant_id = '%s' AND user_id = '%s' AND session_id = '%s' AND %s
                ORDER BY sequence DESC, turn_id DESC
                LIMIT 21`,
				fixtureID.TenantID, fixtureID.UserID, fixtureID.SessionID, keyset),
			want: "turn_rows_keyset",
		},
		{
			name: "exact_older_count_index_only",
			query: fmt.Sprintf(`SELECT count(*) FROM turn_rows
                WHERE tenant_id = '%s' AND user_id = '%s' AND session_id = '%s' AND %s`,
				fixtureID.TenantID, fixtureID.UserID, fixtureID.SessionID, keyset),
			want: "turn_rows_keyset",
		},
		{
			name: "retention_eviction_oldest_first",
			query: fmt.Sprintf(`SELECT turn_id FROM turn_rows
                WHERE tenant_id = '%s' AND user_id = '%s' AND session_id = '%s'
                ORDER BY sequence ASC, turn_id ASC
                LIMIT 50`,
				fixtureID.TenantID, fixtureID.UserID, fixtureID.SessionID),
			want: "turn_rows_keyset",
		},
		{
			name: "identity_effective_agent_session_root_turn",
			query: fmt.Sprintf(`SELECT turn_id FROM turn_rows
                WHERE tenant_id = '%s' AND user_id = '%s' AND session_id = '%s'
                  AND effective_agent_id = 'agent-main' AND turn_id = 'run-00001'`,
				fixtureID.TenantID, fixtureID.UserID, fixtureID.SessionID),
			want: "turn_rows_by_agent",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var plan string
			if err := db.QueryRowContext(ctx, "EXPLAIN "+tc.query).Scan(&plan); err != nil {
				t.Fatalf("EXPLAIN: %v", err)
			}
			if !strings.Contains(plan, "Index Scan using "+tc.want) &&
				!strings.Contains(plan, "Index Only Scan using "+tc.want) &&
				!strings.Contains(plan, "Bitmap Index Scan on "+tc.want) {
				t.Errorf("plan does not resolve through %s:\n%s", tc.want, plan)
			}
			if strings.Contains(plan, "Seq Scan on turn_rows") {
				t.Errorf("plan falls back to a sequential scan (the query is not index-backed):\n%s", plan)
			}
		})
	}
}
