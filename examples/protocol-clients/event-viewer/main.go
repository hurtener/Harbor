// Command event-viewer is the worked client from the Protocol
// adoption track's build-a-client guide
// (docs/site/protocol/build-a-client.md). It is deliberately
// SDK-free: pure stdlib against the Harbor Protocol wire — the point
// is that the Protocol IS the integration surface, no Harbor Go
// import required. The guide quotes this file; the phase smoke
// compile-gates it so the listing cannot rot.
//
// What it does, in order:
//
//  1. Resolves a connection token — HARBOR_TOKEN if set, otherwise
//     the loopback-only dev bootstrap endpoint (`harbor dev` only).
//  2. Calls `runtime.info` and checks Protocol-version compatibility
//     (same major) plus the `events_subscribe` capability.
//  3. Opens the SSE event stream for one session and prints every
//     frame: sequence, type, run, and the payload JSON.
//
// Usage:
//
//	harbor dev &
//	go run ./examples/protocol-clients/event-viewer
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
)

func main() {
	baseURL := envOr("HARBOR_BASE_URL", "http://127.0.0.1:18080")
	session := envOr("HARBOR_SESSION", "event-viewer")

	// 1. A token authenticates (tenant, user) + scopes; the session is
	// chosen per-request via the X-Harbor-Session header.
	token := os.Getenv("HARBOR_TOKEN")
	if token == "" {
		var boot struct {
			Token string `json:"token"`
		}
		mustPost(baseURL+"/v1/dev/bootstrap.json", "", "", []byte(`{}`), &boot)
		token = boot.Token
	}

	// 2. runtime.info — the attach handshake. Pin what you were built
	// against; tolerate what you don't know (see the versioning guide).
	var info struct {
		InstanceID      string   `json:"instance_id"`
		BuildVersion    string   `json:"build_version"`
		ProtocolVersion string   `json:"protocol_version"`
		Capabilities    []string `json:"capabilities"`
	}
	mustPost(baseURL+"/v1/control/runtime.info", token, session, []byte(`{"identity":{}}`), &info)
	fmt.Printf("runtime %s (build %s) speaks Protocol %s\n",
		info.InstanceID, info.BuildVersion, info.ProtocolVersion)
	const builtAgainstMajor = "0" // Protocol 0.x — same major ⇒ compatible
	if strings.Split(info.ProtocolVersion, ".")[0] != builtAgainstMajor {
		fail("protocol major version skew: runtime speaks %s, this client was built against %s.x",
			info.ProtocolVersion, builtAgainstMajor)
	}
	capable := false
	for _, c := range info.Capabilities {
		capable = capable || c == "events_subscribe"
	}
	if !capable {
		fail("this runtime does not advertise the events_subscribe capability")
	}

	// 3. The SSE stream. Last-Event-ID: 0 replays the session's
	// retained history before live-tailing.
	req, err := http.NewRequest(http.MethodGet, baseURL+"/v1/events", nil)
	if err != nil {
		fail("build events request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Harbor-Session", session)
	req.Header.Set("Last-Event-ID", "0")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fail("open event stream: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		fail("event stream: HTTP %d", resp.StatusCode)
	}
	fmt.Printf("tailing session %q — Ctrl-C to stop\n", session)

	// Each SSE frame is `event:` + `id:` + `data:` lines, blank-line
	// terminated. The data object carries the identity and payload.
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var eventType, id string
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			eventType = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "id: "):
			id = strings.TrimPrefix(line, "id: ")
		case strings.HasPrefix(line, "data: "):
			var frame struct {
				Run     string          `json:"run"`
				Payload json.RawMessage `json:"payload"`
			}
			data := strings.TrimPrefix(line, "data: ")
			if err := json.Unmarshal([]byte(data), &frame); err != nil {
				fail("malformed data frame: %v", err) // a broken frame is a wire bug — fail loud
			}
			fmt.Printf("%6s  %-28s run=%-26s %s\n", id, eventType, frame.Run, frame.Payload)
		}
	}
	if err := scanner.Err(); err != nil {
		fail("event stream closed: %v", err)
	}
}

// mustPost issues an identity-bearing Protocol POST and decodes the
// JSON response into out, failing loudly on any non-200.
func mustPost(url, token, session string, body []byte, out any) {
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		fail("build request %s: %v", url, err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if session != "" {
		req.Header.Set("X-Harbor-Session", session)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fail("%s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		fail("%s: HTTP %d", url, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		fail("%s: decode response: %v", url, err)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "event-viewer: "+format+"\n", args...)
	os.Exit(1)
}
