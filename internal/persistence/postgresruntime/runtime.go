// Package postgresruntime composes Harbor's six PostgreSQL projections over
// one runtime-owned aggregate pool manager. It is deliberately separate from
// the individual drivers: drivers expose injected-DB constructors, while
// this package owns runtime topology and lifecycle.
package postgresruntime

import (
	"context"
	"fmt"

	"github.com/hurtener/Harbor/internal/artifacts"
	artifactspg "github.com/hurtener/Harbor/internal/artifacts/drivers/postgres"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/embeddings"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/memory"
	memorypg "github.com/hurtener/Harbor/internal/memory/drivers/postgres"
	rollups "github.com/hurtener/Harbor/internal/observability/rollups"
	rollupspg "github.com/hurtener/Harbor/internal/observability/rollups/drivers/postgres"
	"github.com/hurtener/Harbor/internal/persistence/postgrespool"
	"github.com/hurtener/Harbor/internal/sessions/turns"
	turnspg "github.com/hurtener/Harbor/internal/sessions/turns/drivers/postgres"
	"github.com/hurtener/Harbor/internal/skills"
	skillspg "github.com/hurtener/Harbor/internal/skills/drivers/postgres"
	"github.com/hurtener/Harbor/internal/state"
	statepg "github.com/hurtener/Harbor/internal/state/drivers/postgres"
)

// Dependencies are the non-storage collaborators needed by memory and
// skills. The event bus and state store are supplied by the caller because
// state must be opened before events and memory in the runtime graph.
type Dependencies struct {
	Bus        events.EventBus
	State      state.StateStore
	Summarizer memory.Summarizer
	Embedder   embeddings.Embedder
}

// Runtime owns the aggregate pool manager and exposes injected constructors
// for enabled PostgreSQL stores. Store closers never close the borrowed pools;
// Close is registered after store closers in the caller's reverse chain.
type Runtime struct {
	Pools *postgrespool.Manager
}

// Subsystem is a Harbor-owned PostgreSQL projection included in the runtime
// pool contract. Keep this closed set exhaustive: adding a PostgreSQL driver
// requires adding its DSN selector here and its migration/schema contract in
// the driver package.
type Subsystem string

const (
	SubsystemState                Subsystem = "state"
	SubsystemMemory               Subsystem = "memory"
	SubsystemArtifacts            Subsystem = "artifacts"
	SubsystemSkills               Subsystem = "skills"
	SubsystemSessionsTurns        Subsystem = "sessions.turns"
	SubsystemObservabilityRollups Subsystem = "observability.rollups"
)

var allSubsystems = [...]Subsystem{
	SubsystemState,
	SubsystemMemory,
	SubsystemArtifacts,
	SubsystemSkills,
	SubsystemSessionsTurns,
	SubsystemObservabilityRollups,
}

// ManagedSubsystems returns the closed set of PostgreSQL projections that
// must participate in the runtime-owned pool and migration contract.
func ManagedSubsystems() []Subsystem {
	return append([]Subsystem(nil), allSubsystems[:]...)
}

// PoolSpecs returns one spec for every enabled PostgreSQL projection. It is
// the single registry used by Open, so equal DSNs are deduplicated by the
// pool manager and distinct DSNs remain under the same aggregate budget.
func PoolSpecs(cfg *config.Config) []postgrespool.Spec {
	if cfg == nil {
		return nil
	}
	result := make([]postgrespool.Spec, 0, len(allSubsystems))
	for _, subsystem := range allSubsystems {
		var driver, dsn string
		switch subsystem {
		case SubsystemState:
			driver, dsn = cfg.State.Driver, cfg.State.DSN
		case SubsystemMemory:
			driver, dsn = cfg.Memory.Driver, cfg.Memory.DSN
		case SubsystemArtifacts:
			driver, dsn = cfg.Artifacts.Driver, cfg.Artifacts.DSN
		case SubsystemSkills:
			driver, dsn = cfg.Skills.Driver, cfg.Skills.DSN
		case SubsystemSessionsTurns:
			driver, dsn = cfg.Sessions.Turns.Driver, cfg.Sessions.Turns.DSN
		case SubsystemObservabilityRollups:
			driver, dsn = cfg.Observability.Rollups.Driver, cfg.Observability.Rollups.DSN
		}
		if driver == "postgres" {
			result = append(result, postgrespool.Spec{Subsystem: string(subsystem), DSN: dsn})
		}
	}
	return result
}

// Open creates the runtime pool manager for every enabled PostgreSQL store,
// including turns and rollups even though those stores are opened later by
// the serve band. Equal DSNs share one physical *sql.DB; distinct DSNs use
// the same aggregate budget.
func Open(ctx context.Context, cfg *config.Config) (*Runtime, error) {
	if cfg == nil {
		return nil, fmt.Errorf("postgres runtime: config is required")
	}
	p := cfg.Postgres
	manager, err := postgrespool.Open(ctx, postgrespool.Config{
		MaxOpenConns:    p.MaxOpenConns,
		MaxIdleConns:    p.MaxIdleConns,
		ConnMaxLifetime: p.ConnMaxLifetime,
		ConnMaxIdleTime: p.ConnMaxIdleTime,
	}, PoolSpecs(cfg))
	if err != nil {
		return nil, err
	}
	return &Runtime{Pools: manager}, nil
}

// State opens the configured state store, borrowing a runtime pool when the
// Postgres driver is selected.
func (r *Runtime) State(ctx context.Context, cfg config.StateConfig) (state.StateStore, error) {
	if cfg.Driver != "postgres" {
		return nil, fmt.Errorf("postgres runtime: State called for non-postgres driver %q", cfg.Driver)
	}
	db, ok := r.Pools.DB(cfg.DSN)
	if !ok {
		return nil, fmt.Errorf("postgres runtime: no pool for state DSN")
	}
	return statepg.NewWithDB(cfg, db)
}

// Artifacts opens the configured artifact store over the borrowed pool.
func (r *Runtime) Artifacts(cfg config.ArtifactsConfig) (artifacts.ArtifactStore, error) {
	db, ok := r.Pools.DB(cfg.DSN)
	if !ok {
		return nil, fmt.Errorf("postgres runtime: no pool for artifacts DSN")
	}
	return artifactspg.NewWithDB(cfg, db)
}

// Memory opens the configured memory store over the borrowed pool.
func (r *Runtime) Memory(cfg memory.ConfigSnapshot, deps memory.Deps) (memory.MemoryStore, error) {
	db, ok := r.Pools.DB(cfg.DSN)
	if !ok {
		return nil, fmt.Errorf("postgres runtime: no pool for memory DSN")
	}
	return memorypg.NewWithDB(cfg, deps, db)
}

// Skills opens the configured skills store over the borrowed pool.
func (r *Runtime) Skills(cfg skills.ConfigSnapshot, deps skills.Deps) (skills.SkillStore, error) {
	db, ok := r.Pools.DB(cfg.DSN)
	if !ok {
		return nil, fmt.Errorf("postgres runtime: no pool for skills DSN")
	}
	return skillspg.NewWithDB(cfg, deps, db)
}

// Turns opens the configured turns projection over the borrowed pool.
func (r *Runtime) Turns(cfg turnspg.Config) (turns.Store, error) {
	db, ok := r.Pools.DB(cfg.DSN)
	if !ok {
		return nil, fmt.Errorf("postgres runtime: no pool for sessions.turns DSN")
	}
	return turnspg.NewWithDB(cfg, db)
}

// Rollups opens the configured rollups projection over the borrowed pool.
func (r *Runtime) Rollups(cfg rollupspg.Config) (rollups.Store, error) {
	db, ok := r.Pools.DB(cfg.DSN)
	if !ok {
		return nil, fmt.Errorf("postgres runtime: no pool for observability.rollups DSN")
	}
	return rollupspg.NewWithDB(cfg, db)
}
