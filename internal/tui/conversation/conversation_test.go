package conversation

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/tui/projection"
)

func testToken(t *testing.T, id types.IdentityScope, expiry time.Time) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"ES256","typ":"JWT"}`))
	payload, err := json.Marshal(map[string]any{"tenant": id.Tenant, "user": id.User, "session": id.Session, "exp": expiry.Unix(), "scopes": []string{"admin"}})
	if err != nil {
		t.Fatal(err)
	}
	return header + "." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
}

func TestLifetimeTokenSource_RotationReplacementExpiryAndConcurrentReuse(t *testing.T) {
	id := types.IdentityScope{Tenant: "t", User: "u", Session: "s"}
	now := time.Unix(1000, 0)
	path := filepath.Join(t.TempDir(), "token")
	first := testToken(t, id, now.Add(time.Hour))
	second := testToken(t, id, now.Add(2*time.Hour))
	if err := os.WriteFile(path, []byte(first), 0o600); err != nil {
		t.Fatal(err)
	}
	source := NewTokenSource(path, "")
	source.now = func() time.Time { return now }
	if got, err := source.Token(context.Background(), id); err != nil || got != first {
		t.Fatalf("first=%q %v", got, err)
	}
	if err := os.WriteFile(path, []byte(second), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := source.Token(context.Background(), id); err != nil || got != second {
		t.Fatalf("rotated=%q %v", got, err)
	}
	replacement := testToken(t, id, now.Add(3*time.Hour))
	if err := source.Replace(id, replacement); err != nil {
		t.Fatal(err)
	}
	baseline := runtime.NumGoroutine()
	var wg sync.WaitGroup
	errs := make(chan error, 128)
	for range 128 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := source.Token(context.Background(), id)
			if err != nil {
				errs <- err
			} else if got != replacement {
				errs <- errors.New("cross-talk")
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	eventuallyGoroutinesSettle(t, baseline, 2)
	expired := NewTokenSource("", testToken(t, id, now.Add(-time.Second)))
	expired.now = func() time.Time { return now }
	if _, err := expired.Token(context.Background(), id); !errors.Is(err, ErrTokenExpired) {
		t.Fatalf("expired=%v", err)
	}
}

func TestStore_AtomicBoundedRestoreAndMalformedFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	store := NewStore(path)
	at := time.Unix(2000, 0)
	store.now = func() time.Time { return at }
	id := types.IdentityScope{Tenant: "t", User: "u", Session: "s"}
	want := InteractionState{Identity: id, RuntimeFingerprint: "runtime", Draft: "draft", History: []string{"old"}, Stash: []string{"saved"}, ScrollBlockID: "block", ExpandedTools: []string{"tool"}, SidebarWidth: 42, SidebarOpen: true, Theme: "dark", ReducedMotion: true}
	if err := store.Save(want); err != nil {
		t.Fatal(err)
	}
	got, ok, err := store.Load(id, "runtime")
	if err != nil || !ok || got.Draft != "draft" || got.Identity.Session != "s" || len(got.History) != 1 || len(got.Stash) != 1 {
		t.Fatalf("load=%#v %v %v", got, ok, err)
	}
	last, err := store.LastSession("t", "u", "runtime")
	if err != nil || last != "s" {
		t.Fatalf("last=%q %v", last, err)
	}
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Load(id, "runtime"); err == nil {
		t.Fatal("malformed state silently ignored")
	}
}

func TestStore_ConcurrentAtomicReuseIsBounded(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "state.json"))
	baseline := runtime.NumGoroutine()
	var wg sync.WaitGroup
	errs := make(chan error, 128)
	for i := range 128 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := types.IdentityScope{Tenant: "t", User: "u", Session: fmt.Sprint(i)}
			if err := store.Save(InteractionState{Identity: id, RuntimeFingerprint: "runtime", Draft: fmt.Sprint(i)}); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	file, err := store.read()
	if err != nil || len(file.Entries) != maxStateEntries {
		t.Fatalf("entries=%d err=%v", len(file.Entries), err)
	}
	eventuallyGoroutinesSettle(t, baseline, 2)
}

func TestTranscript_StickySearchNavigationExportAndQueueFailure(t *testing.T) {
	p := projection.Projection{Identity: types.IdentityScope{Tenant: "t", User: "u", Session: "s"}, Blocks: []projection.Block{{ID: "u", Kind: "user", Text: "hello"}, {ID: "r", Kind: "reasoning", Text: "think"}, {ID: "a", Kind: "text", Text: "answer"}}}
	view := NewTranscript(p).Scroll(-1)
	p.Blocks = append(p.Blocks, projection.Block{ID: "tool", Kind: "tool", Tool: "lookup"})
	view = view.Replace(p)
	if view.Follow || view.NewOutput != 1 {
		t.Fatalf("sticky=%#v", view)
	}
	view = view.Search("lookup")
	if len(view.Matches) != 1 || view.Selected != 3 {
		t.Fatalf("search=%#v", view)
	}
	view.ShowReasoning = false
	view.Selected = 0
	view = view.Jump(1)
	if view.Selected != 2 {
		t.Fatalf("jump selected=%d", view.Selected)
	}
	var out bytes.Buffer
	if err := view.Export(&out, ExportOptions{Tools: true, Metadata: true}); err != nil || !bytes.Contains(out.Bytes(), []byte("lookup")) {
		t.Fatalf("export=%q %v", out.String(), err)
	}
	q := Queue{}.Enqueue("one").Enqueue("two")
	var active FollowUp
	q, active, _ = q.Begin()
	q = q.Fail(active.ID, errors.New("network"))
	entries := q.Entries()
	if entries[1].State != "local queue" {
		t.Fatalf("unrelated intent lost: %#v", entries)
	}
}

func TestStore_ExpiryEvictionAndValidationFailures(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	store := NewStore(path)
	now := time.Unix(5000, 0)
	store.now = func() time.Time { return now }
	if err := store.Save(InteractionState{}); err == nil {
		t.Fatal("empty identity saved")
	}
	id := types.IdentityScope{Tenant: "t", User: "u", Session: "s"}
	if err := store.Save(InteractionState{Identity: id}); err == nil {
		t.Fatal("empty fingerprint saved")
	}
	if err := store.Save(InteractionState{Identity: id, RuntimeFingerprint: "r", Draft: string(make([]byte, maxDraftBytes+1))}); err == nil {
		t.Fatal("oversize draft saved")
	}
	if err := store.Save(InteractionState{Identity: id, RuntimeFingerprint: "r", History: []string{string(make([]byte, maxListBytes+1))}}); err == nil {
		t.Fatal("oversize history saved")
	}
	if err := store.Save(InteractionState{Identity: id, RuntimeFingerprint: "r", ExpandedTools: make([]string, maxCollapsedIDs+1)}); err == nil {
		t.Fatal("oversize collapsed state saved")
	}
	for i := range maxStateEntries + 10 {
		store.now = func() time.Time { return now.Add(time.Duration(i) * time.Minute) }
		entry := InteractionState{Identity: types.IdentityScope{Tenant: "t", User: "u", Session: fmt.Sprint(i)}, RuntimeFingerprint: "r"}
		if err := store.Save(entry); err != nil {
			t.Fatal(err)
		}
	}
	file, err := store.read()
	if err != nil {
		t.Fatal(err)
	}
	if len(file.Entries) != maxStateEntries {
		t.Fatalf("entries=%d", len(file.Entries))
	}
	store.now = func() time.Time { return now.Add(stateTTL + 100*time.Hour) }
	if _, ok, err := store.Load(types.IdentityScope{Tenant: "t", User: "u", Session: "10"}, "r"); err != nil || ok {
		t.Fatalf("expired ok=%v err=%v", ok, err)
	}
	if last, err := store.LastSession("t", "u", "r"); err != nil || last != "" {
		t.Fatalf("expired last session=%q err=%v", last, err)
	}
	if err := os.WriteFile(path, []byte(`{"version":99,"entries":{},"last_session":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.read(); err == nil {
		t.Fatal("unsupported version accepted")
	}
}

func TestStore_MigratesLegacyEnvelopeAndReportsPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte(`{"version":0}`), 0o600); err != nil {
		t.Fatal(err)
	}
	store := NewStore(path)
	id := types.IdentityScope{Tenant: "t", User: "u", Session: "s"}
	if _, ok, err := store.Load(id, "runtime"); err != nil || ok {
		t.Fatalf("legacy envelope migration ok=%v err=%v", ok, err)
	}
	if runtime.GOOS == "windows" {
		return
	}
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })
	if _, _, err := store.Load(id, "runtime"); err == nil {
		t.Fatal("unreadable state file was silently ignored")
	}
}

func TestStore_RejectsPermissiveStateFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("reason: POSIX state-file permissions are not available")
	}
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"entries":{},"last_session":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := NewStore(path).LastSession("t", "u", "runtime"); err == nil || !strings.Contains(err.Error(), "too permissive") {
		t.Fatalf("permissive file error=%v", err)
	}
}

func TestStore_SaveSecuresParentDirectoryAndStateFile(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(parent, "tui.json")
	store := NewStore(path)
	value := InteractionState{Identity: types.IdentityScope{Tenant: "t", User: "u", Session: "s"}, RuntimeFingerprint: "runtime"}
	if err := store.Save(value); err != nil {
		t.Fatal(err)
	}
	parentInfo, err := os.Stat(parent)
	if err != nil {
		t.Fatal(err)
	}
	fileInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if parentInfo.Mode().Perm() != 0o700 || fileInfo.Mode().Perm() != 0o600 {
		t.Fatalf("permissions parent=%04o file=%04o", parentInfo.Mode().Perm(), fileInfo.Mode().Perm())
	}
}

func TestStore_SaveRejectsSymbolicLinkParent(t *testing.T) {
	root := t.TempDir()
	realParent := filepath.Join(root, "real")
	if err := os.Mkdir(realParent, 0o700); err != nil {
		t.Fatal(err)
	}
	linkedParent := filepath.Join(root, "linked")
	if err := os.Symlink(realParent, linkedParent); err != nil {
		t.Fatal(err)
	}
	store := NewStore(filepath.Join(linkedParent, "state.json"))
	err := store.Save(InteractionState{Identity: types.IdentityScope{Tenant: "t", User: "u", Session: "s"}, RuntimeFingerprint: "runtime"})
	if err == nil || !strings.Contains(err.Error(), "symbolic-link state directory") {
		t.Fatalf("symbolic-link parent error=%v", err)
	}
}

func TestStore_RejectsStructurallyValidMismatchedEntry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	id := types.IdentityScope{Tenant: "t", User: "u", Session: "s"}
	key := stateKey(id, "runtime")
	body, err := json.Marshal(stateFile{Version: stateVersion, LastSession: map[string]string{}, Entries: map[string]InteractionState{key: {Identity: types.IdentityScope{Tenant: "t", User: "u", Session: "other"}, RuntimeFingerprint: "runtime", UpdatedAt: time.Now()}}})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err = NewStore(path).Load(id, "runtime"); err == nil {
		t.Fatal("mismatched state entry was silently accepted")
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }

func TestTranscript_EmptyBoundsExportErrorsAndQueueRemoval(t *testing.T) {
	empty := NewTranscript(projection.Projection{}).Scroll(1).Jump(1).Search("")
	if empty.Selected != 0 {
		t.Fatal("empty moved")
	}
	if err := empty.Export(&bytes.Buffer{}, ExportOptions{}); !errors.Is(err, ErrExportEmpty) {
		t.Fatalf("empty export=%v", err)
	}
	p := projection.Projection{Identity: types.IdentityScope{Tenant: "t", User: "u", Session: "s"}, Blocks: []projection.Block{{Kind: "reasoning", Text: "r"}, {Kind: "event", EventType: "unknown"}}}
	if err := NewTranscript(p).Export(failingWriter{}, ExportOptions{Reasoning: true, RawEvents: true}); !errors.Is(err, ErrExportWrite) {
		t.Fatalf("write=%v", err)
	}
	q := Queue{}.Enqueue("one").Enqueue("two")
	id := q.Entries()[0].ID
	q = q.Remove(id)
	if len(q.Entries()) != 1 {
		t.Fatal("remove failed")
	}
	q, _, _ = q.Begin()
	q = q.Remove(q.Entries()[0].ID)
	if len(q.Entries()) != 1 {
		t.Fatal("dispatching entry removed")
	}
	dispatching := q.Entries()[0].ID
	q = q.Complete(dispatching)
	if len(q.Entries()) != 0 {
		t.Fatal("completed dispatch was retained")
	}
	if len(q.Complete("missing").Entries()) != 0 {
		t.Fatal("missing completion changed queue")
	}
}

func TestToken_MalformedMismatchCancellationAndMissingFile(t *testing.T) {
	now := time.Now()
	id := types.IdentityScope{Tenant: "t", User: "u", Session: "s"}
	for _, token := range []string{"bad", "a.!!!.c"} {
		if _, err := ParseToken(token, now); err == nil {
			t.Fatalf("accepted %q", token)
		}
	}
	other := id
	other.Session = "other"
	source := NewTokenSource("", testToken(t, other, now.Add(time.Hour)))
	source.now = func() time.Time { return now }
	if _, err := source.Token(context.Background(), id); err == nil {
		t.Fatal("mismatch accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := source.Token(ctx, id); err == nil {
		t.Fatal("cancel ignored")
	}
	missing := NewTokenSource(filepath.Join(t.TempDir(), "missing"), "")
	if _, err := missing.Token(context.Background(), id); err == nil {
		t.Fatal("missing file ignored")
	}
	if err := source.Replace(types.IdentityScope{}, "x"); err == nil {
		t.Fatal("invalid replacement accepted")
	}
}

func TestToken_JSONMapFallbackAndUnavailable(t *testing.T) {
	now := time.Now()
	id := types.IdentityScope{Tenant: "t", User: "u", Session: "s"}
	token := testToken(t, id, now.Add(time.Hour))
	path := filepath.Join(t.TempDir(), "tokens")
	body, _ := json.Marshal(map[string]string{scopeKey(id): token})
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	source := NewTokenSource(path, "")
	source.now = func() time.Time { return now }
	if got, err := source.Token(t.Context(), id); err != nil || got != token {
		t.Fatalf("map=%q %v", got, err)
	}
	other := id
	other.Session = "other"
	if _, err := source.Token(t.Context(), other); !errors.Is(err, ErrTokenUnavailable) {
		t.Fatalf("unavailable=%v", err)
	}
	fallback := NewTokenSource(filepath.Join(t.TempDir(), "absent"), token)
	fallback.now = func() time.Time { return now }
	if _, err := fallback.Token(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if _, err := NewTokenSource("", "").Token(t.Context(), id); !errors.Is(err, ErrTokenUnavailable) {
		t.Fatalf("empty=%v", err)
	}
}

func TestConversation_RemainingBoundsAndMalformedBranches(t *testing.T) {
	now := time.Now()
	id := types.IdentityScope{Tenant: "t", User: "u", Session: "s"}
	encode := func(payload string) string {
		return "e30." + base64.RawURLEncoding.EncodeToString([]byte(payload)) + ".sig"
	}
	for _, token := range []string{encode("{"), encode(`{"tenant":"t","user":"u","session":"s"}`)} {
		if _, err := ParseToken(token, now); err == nil {
			t.Fatalf("accepted malformed claims %q", token)
		}
	}
	path := filepath.Join(t.TempDir(), "tokens")
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	source := NewTokenSource(path, "")
	source.now = func() time.Time { return now }
	if _, err := source.Token(t.Context(), id); err == nil {
		t.Fatal("malformed token map accepted")
	}
	statePath := filepath.Join(t.TempDir(), "state")
	if err := os.WriteFile(statePath, []byte(`{"version":1,"entries":null,"last_session":null}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewStore(statePath).LastSession("t", "u", "r"); err == nil {
		t.Fatal("nil state maps accepted")
	}
	p := projection.Projection{Identity: id, Blocks: []projection.Block{{Kind: "reasoning", Text: "hidden"}, {Kind: "tool", Tool: "hidden-tool"}, {Kind: "event", EventType: "raw"}}}
	view := NewTranscript(p)
	var out bytes.Buffer
	if err := view.Export(&out, ExportOptions{}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "hidden") || strings.Contains(out.String(), "raw") {
		t.Fatalf("detail leaked: %q", out.String())
	}
	view = view.Scroll(-99)
	p.Blocks = append(p.Blocks, projection.Block{Kind: "text", Text: "new"})
	view = view.Replace(p)
	if view.Follow || view.NewOutput == 0 {
		t.Fatalf("new output=%#v", view)
	}
	q := Queue{}
	for i := range 60 {
		q = q.Enqueue(fmt.Sprint(i))
	}
	if len(q.Entries()) != 50 {
		t.Fatalf("queue bound=%d", len(q.Entries()))
	}
	q, active, ok := q.Begin()
	if !ok {
		t.Fatal("begin failed")
	}
	q = q.Fail(active.ID, errors.New("stop"))
	if _, _, ok = q.Begin(); ok {
		t.Fatal("paused queue dispatched")
	}
}

func TestNotifier_SustainedHighRateCoalescesOnlyBatchableUpdates(t *testing.T) {
	notifier := NewNotifier(4)
	defer notifier.Close()
	id := types.IdentityScope{Tenant: "t", User: "u", Session: "s"}
	done := make(chan struct{})
	go func() {
		for i := 1; i <= 10000; i++ {
			notifier.Notify(Update{Identity: id, Generation: 1, State: StateLive, Batchable: true, Projection: projection.Projection{Identity: id, Generation: 1, LastSequence: uint64(i)}})
		}
		for i := 1; i <= 100; i++ {
			notifier.Notify(Update{Identity: id, Generation: 1, State: StateReplaying, Attempt: i})
		}
		close(done)
	}()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	critical := 0
	latest := uint64(0)
	overflow := false
	for critical < 100 || latest < 10000 {
		update, ok := notifier.Next(ctx)
		if !ok {
			t.Fatal("notifier closed before complete state")
		}
		if update.Batchable {
			latest = update.Projection.LastSequence
			overflow = overflow || update.Overflow
		} else if update.State == StateReplaying {
			critical++
		}
	}
	<-done
	if critical != 100 || latest != 10000 || !overflow {
		t.Fatalf("critical=%d latest=%d overflow=%v", critical, latest, overflow)
	}
}

func TestQueueRecoveryAndChannelSourceBranches(t *testing.T) {
	var queue Queue
	for i := range 55 {
		queue = queue.EnqueueTurn(fmt.Sprintf("turn-%d", i), []string{"artifact"}, map[string]string{"artifact": "ref"})
	}
	if entries := queue.Entries(); len(entries) != 50 || entries[0].Text != "turn-5" {
		t.Fatalf("bounded queue=%#v", entries)
	}
	next, first, ok := queue.Begin()
	if !ok || first.Text != "turn-5" {
		t.Fatalf("begin=%#v ok=%v", first, ok)
	}
	queue = next.Fail(first.ID, errors.New("retry"))
	if _, _, ok = queue.Begin(); ok {
		t.Fatal("failed queue did not pause ordered dispatch")
	}
	queue = queue.Retry(first.ID)
	next, retried, ok := queue.Begin()
	if !ok || retried.ID != first.ID {
		t.Fatalf("retry=%#v ok=%v", retried, ok)
	}
	queue = next.Complete(retried.ID)
	entries := queue.Entries()
	queue = queue.Discard(entries[0].ID)
	if len(queue.Entries()) != len(entries)-1 {
		t.Fatal("local discard did not remove one entry")
	}
	next, failed, ok := queue.Begin()
	if !ok {
		t.Fatal("second recovery entry unavailable")
	}
	queue = next.Fail(failed.ID, errors.New("discard"))
	queue = queue.Discard(failed.ID)
	if _, _, ok = queue.Begin(); !ok {
		t.Fatal("failed discard did not resume later ordered intent")
	}

	updates := make(chan Update, 1)
	source := ChannelSource(updates)
	want := Update{Generation: 7, State: StateLive}
	updates <- want
	if got, received := source.Next(t.Context()); !received || got.Generation != want.Generation {
		t.Fatalf("channel update=%#v received=%v", got, received)
	}
	close(updates)
	if _, received := source.Next(t.Context()); received {
		t.Fatal("closed channel reported an update")
	}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, received := ChannelSource(make(chan Update)).Next(cancelled); received {
		t.Fatal("cancelled channel source reported an update")
	}
	if notifier := NewNotifier(0); cap(notifier.critical) != 1 {
		t.Fatalf("minimum notifier capacity=%d", cap(notifier.critical))
	}
}

func TestCandidateTokenSource_UsesCandidateUntilCommit(t *testing.T) {
	now := time.Now()
	id := types.IdentityScope{Tenant: "t", User: "u", Session: "candidate"}
	fileToken := testToken(t, id, now.Add(time.Hour))
	candidate := testToken(t, id, now.Add(2*time.Hour))
	base := NewTokenSource("", fileToken)
	base.now = func() time.Time { return now }
	if err := base.Replace(types.IdentityScope{}, candidate); err == nil {
		t.Fatal("replacement accepted an incomplete identity")
	}
	source := &candidateTokenSource{scope: id, token: candidate, base: base}
	if got, err := source.Token(t.Context(), id); err != nil || got != candidate {
		t.Fatalf("candidate token=%q err=%v", got, err)
	}
	other := id
	other.Session = "other"
	if _, err := source.Token(t.Context(), other); !errors.Is(err, ErrTokenUnavailable) {
		t.Fatalf("cross-scope candidate error=%v", err)
	}
	source.committed.Store(true)
	if got, err := source.Token(t.Context(), id); err != nil || got != fileToken {
		t.Fatalf("committed source token=%q err=%v", got, err)
	}
}
