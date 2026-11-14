package engine_test

import (
	"errors"
	"sync"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/tasks"
)

// RequireIdleSession admits a start only while the session triple has no
// non-terminal top-level foreground task; an exact retry still answers its
// task; background and child tasks, and other sessions, do not count.
func TestSpawn_RequireIdleSession(t *testing.T) {
	eng := newCMEngine(t)
	ctx := cmCtx(t)
	idle := func(key string) tasks.SpawnRequest {
		return tasks.SpawnRequest{Identity: cmQuad(), Kind: tasks.KindForeground, Query: "q-" + key, IdempotencyKey: key, RequireIdleSession: true}
	}
	first, err := eng.Spawn(ctx, idle("a"))
	if err != nil {
		t.Fatalf("first spawn: %v", err)
	}
	if _, err := eng.Spawn(ctx, idle("b")); !errors.Is(err, tasks.ErrSessionBusy) {
		t.Fatalf("second spawn while pending = %v, want ErrSessionBusy", err)
	}
	// An exact retry of the accepted start answers its task, never busy.
	again, err := eng.Spawn(ctx, idle("a"))
	if err != nil || again.ID != first.ID || !again.Reused {
		t.Fatalf("retry = %+v, %v; want the same task reused", again, err)
	}
	// Without the flag the ordinary behaviour is unchanged.
	if _, err := eng.Spawn(ctx, tasks.SpawnRequest{Identity: cmQuad(), Kind: tasks.KindForeground, Query: "free"}); err != nil {
		t.Fatalf("ordinary spawn: %v", err)
	}
	// Running and paused still hold the session.
	if err := eng.MarkRunning(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	if err := eng.MarkPaused(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Spawn(ctx, idle("c")); !errors.Is(err, tasks.ErrSessionBusy) {
		t.Fatalf("spawn while paused = %v, want ErrSessionBusy", err)
	}
	// Another session of the same user is independent.
	other := cmQuad()
	other.SessionID = "s-other"
	otherCtx, err := identity.With(ctx, other.Identity)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Spawn(otherCtx, tasks.SpawnRequest{Identity: other, Kind: tasks.KindForeground, Query: "o", RequireIdleSession: true}); err != nil {
		t.Fatalf("other session: %v", err)
	}
}

// A lingering background task or child task never holds the session.
func TestSpawn_RequireIdleSessionIgnoresBackgroundAndChildren(t *testing.T) {
	eng := newCMEngine(t)
	ctx := cmCtx(t)
	parent, err := eng.Spawn(ctx, tasks.SpawnRequest{Identity: cmQuad(), Kind: tasks.KindForeground, Query: "parent"})
	if err != nil {
		t.Fatal(err)
	}
	parentID := parent.ID
	if _, err := eng.Spawn(ctx, tasks.SpawnRequest{Identity: cmQuad(), Kind: tasks.KindForeground, Query: "child", ParentTaskID: &parentID}); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Spawn(ctx, tasks.SpawnRequest{Identity: cmQuad(), Kind: tasks.KindBackground, Query: "bg"}); err != nil {
		t.Fatal(err)
	}
	if err := eng.MarkRunning(ctx, parent.ID); err != nil {
		t.Fatal(err)
	}
	if err := eng.MarkComplete(ctx, parent.ID, tasks.TaskResult{}); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Spawn(ctx, tasks.SpawnRequest{Identity: cmQuad(), Kind: tasks.KindForeground, Query: "next", RequireIdleSession: true}); err != nil {
		t.Fatalf("spawn beside a child and a background task = %v, want admitted", err)
	}
}

// Twenty concurrent flagged starts in one idle session create exactly one
// task.
func TestSpawn_RequireIdleSessionConcurrentStartsOnce(t *testing.T) {
	eng := newCMEngine(t)
	ctx := cmCtx(t)
	var wg sync.WaitGroup
	var mu sync.Mutex
	admitted, busy := 0, 0
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := eng.Spawn(ctx, tasks.SpawnRequest{Identity: cmQuad(), Kind: tasks.KindForeground, Query: "q", IdempotencyKey: string(rune('a' + i)), RequireIdleSession: true})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				admitted++
			case errors.Is(err, tasks.ErrSessionBusy):
				busy++
			default:
				t.Errorf("spawn: %v", err)
			}
		}(i)
	}
	wg.Wait()
	if admitted != 1 || busy != 19 {
		t.Fatalf("admitted=%d busy=%d, want 1 and 19", admitted, busy)
	}
}
