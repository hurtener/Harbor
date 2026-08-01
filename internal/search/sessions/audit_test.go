package sessions_test

import (
	"context"

	"github.com/hurtener/Harbor/internal/events"
)

func testAudit(context.Context, events.Event) error { return nil }
