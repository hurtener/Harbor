package events_test

import (
	"context"

	eventsubsys "github.com/hurtener/Harbor/internal/events"
)

func testAudit(context.Context, eventsubsys.Event) error { return nil }
