package durable_test

import (
	"context"
	"strings"
	"testing"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/tasks"
)

// TestDurable_NilStore_FailsLoud asserts the durable driver refuses to
// start without a StateStore rather than silently degrading to a
// non-durable backend — an operator who selected durable must get
// durability (CLAUDE.md §13 "no silent stub default").
func TestDurable_NilStore_FailsLoud(t *testing.T) {
	bus := mkBus(t)
	defer func() { _ = bus.Close(context.Background()) }()

	_, err := tasks.OpenDriver("durable", tasks.Dependencies{
		Store:    nil,
		Bus:      bus,
		Redactor: auditpatterns.New(),
		Cfg:      config.TasksConfig{Driver: "durable"},
	})
	if err == nil {
		t.Fatal("durable driver with nil StateStore: want fail-loud error, got nil")
	}
	if !strings.Contains(err.Error(), "StateStore") {
		t.Errorf("error should name the missing StateStore, got: %v", err)
	}
}
