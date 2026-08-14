package stream_test

// Focused tests for the initial-resume cursor (HA-64 / D-425) — the
// narrowly named `?resume_seq=` query parameter that carries the durable
// turn page's live_resume_seq as the SNAPSHOT-TO-LIVE handoff cursor.
//
// These pin the three wire contracts the Console reopen depends on:
//
//  1. an initial query cursor replays events STRICTLY newer than the
//     snapshot through the same bounded Replay path a reconnect uses;
//  2. a malformed initial cursor fails the request closed with a typed
//     Protocol error (400 `invalid_request`) before any subscription
//     opens — never a silent fresh start that drops the gap;
//  3. a valid reconnect Last-Event-ID header OVERRIDES the initial query
//     cursor, so a browser reconnect replays from its own cursor, never
//     from the original snapshot forever.

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/transports/stream"
)

// TestServeHTTP_InitialResume_ReplaysStrictlyNewer — an initial
// `?resume_seq=<n>` cursor replays exactly the events with a sequence
// strictly greater than n (the durable fold's live_resume_seq is the
// exclusive subscribe-from cursor), oldest-first before the live tail.
func TestServeHTTP_InitialResume_ReplaysStrictlyNewer(t *testing.T) {
	bus := newTestBus(t)
	srv := newStreamServer(t, bus)

	id := identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"}
	// Three events land in the replay ring BEFORE any connection.
	publishCancelled(t, bus, id, "r1")
	publishCancelled(t, bus, id, "r2")
	publishCancelled(t, bus, id, "r3")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(
		ctx,
		http.MethodGet,
		srv.URL+"/v1/events?"+stream.InitialResumeQuery+"=1",
		nil,
	)
	req.Header.Set(stream.HeaderTenant, id.TenantID)
	req.Header.Set(stream.HeaderUser, id.UserID)
	req.Header.Set(stream.HeaderSession, id.SessionID)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	// Replay from snapshot seq 1 — events at seq 2 and 3 are replayed;
	// seq 1 itself (at-or-before the cursor) never is.
	deadline := time.Now().Add(2 * time.Second)
	sc := bufio.NewScanner(resp.Body)
	seenSeqs := map[string]bool{}
	for sc.Scan() && time.Now().Before(deadline) {
		line := sc.Text()
		if strings.HasPrefix(line, "id: ") {
			seenSeqs[strings.TrimPrefix(line, "id: ")] = true
		}
		if seenSeqs["2"] && seenSeqs["3"] {
			break
		}
	}
	if !seenSeqs["2"] || !seenSeqs["3"] {
		t.Fatalf("initial-resume replay missing events: seen=%v, want seq 2 and 3", seenSeqs)
	}
	if seenSeqs["1"] {
		t.Error("initial-resume replayed an event at-or-before the cursor (seq 1)")
	}
}

// TestServeHTTP_InitialResume_Malformed_FailsLoud — a present-but-
// malformed initial resume value (empty, non-decimal, negative,
// overflowing uint64, or duplicated) fails the request closed with the
// canonical Protocol error envelope 400 `invalid_request` BEFORE any
// subscription opens. No silent fresh start, no 200-with-a-drop.
func TestServeHTTP_InitialResume_Malformed_FailsLoud(t *testing.T) {
	bus := newTestBus(t)
	srv := newStreamServer(t, bus)

	cases := map[string]string{
		"non-numeric": "abc",
		"negative":    "-1",
		"overflow":    "18446744073709551616", // 2^64 — out of uint64 range
		"empty":       "",
		"duplicated":  "1&" + stream.InitialResumeQuery + "=2",
	}
	for name, value := range cases {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			req, _ := http.NewRequestWithContext(
				ctx,
				http.MethodGet,
				srv.URL+"/v1/events?"+stream.InitialResumeQuery+"="+value,
				nil,
			)
			req.Header.Set(stream.HeaderTenant, "t1")
			req.Header.Set(stream.HeaderUser, "u1")
			req.Header.Set(stream.HeaderSession, "s1")

			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", resp.StatusCode)
			}
			if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
				t.Fatalf("Content-Type = %q, want application/json (not a committed SSE stream)", ct)
			}
			body, _ := io.ReadAll(resp.Body)
			if !strings.Contains(string(body), `"code":"invalid_request"`) {
				t.Errorf("body missing invalid_request code: %q", string(body))
			}
		})
	}
}

// TestServeHTTP_InitialResume_LastEventIDWins — when BOTH the reconnect
// Last-Event-ID header and the initial ?resume_seq= query cursor are
// present, the header takes precedence: replay anchors on the browser's
// own cursor (strictly newer than the header), never on the fold
// snapshot, so a reconnect does not re-deliver frames the client already
// consumed.
func TestServeHTTP_InitialResume_LastEventIDWins(t *testing.T) {
	bus := newTestBus(t)
	srv := newStreamServer(t, bus)

	id := identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"}
	publishCancelled(t, bus, id, "r1")
	publishCancelled(t, bus, id, "r2")
	publishCancelled(t, bus, id, "r3")
	publishCancelled(t, bus, id, "r4")
	publishCancelled(t, bus, id, "r5")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Query says "from 1"; the reconnect header says "from 3".
	req, _ := http.NewRequestWithContext(
		ctx,
		http.MethodGet,
		srv.URL+"/v1/events?"+stream.InitialResumeQuery+"=1",
		nil,
	)
	req.Header.Set(stream.HeaderTenant, id.TenantID)
	req.Header.Set(stream.HeaderUser, id.UserID)
	req.Header.Set(stream.HeaderSession, id.SessionID)
	req.Header.Set("Last-Event-ID", "3")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// The header cursor wins: seq 4 and 5 replay; seq 2 and 3 (which a
	// snapshot-from-1 replay would include) never do.
	deadline := time.Now().Add(2 * time.Second)
	sc := bufio.NewScanner(resp.Body)
	seenSeqs := map[string]bool{}
	for sc.Scan() && time.Now().Before(deadline) {
		line := sc.Text()
		if strings.HasPrefix(line, "id: ") {
			seenSeqs[strings.TrimPrefix(line, "id: ")] = true
		}
		if seenSeqs["4"] && seenSeqs["5"] {
			break
		}
	}
	if !seenSeqs["4"] || !seenSeqs["5"] {
		t.Fatalf("header-cursor replay missing events: seen=%v, want seq 4 and 5", seenSeqs)
	}
	if seenSeqs["2"] || seenSeqs["3"] {
		t.Errorf("header-cursor replay ignored Last-Event-ID and replayed from the query cursor: seen=%v", seenSeqs)
	}
	if seenSeqs["1"] {
		t.Errorf("replay included seq 1 (at-or-before every cursor): seen=%v", seenSeqs)
	}
}
