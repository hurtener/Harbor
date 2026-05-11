package a2a

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/hurtener/Harbor/internal/distributed/a2a"
)

// AgentCardPath is the canonical discovery path appended to a peer's
// base URL. Per the A2A spec: `GET <peer>/.well-known/agent-card.json`.
const AgentCardPath = "/.well-known/agent-card.json"

// defaultAgentCardTTL is the package-wide AgentCard cache TTL when
// neither the driver nor a peer overrides it.
const defaultAgentCardTTL = 10 * time.Minute

// agentCardEntry pairs a fetched card with its observed expiry.
type agentCardEntry struct {
	card    *a2a.AgentCard
	expires time.Time
}

// agentCardCache is the per-driver TTL cache for fetched AgentCards.
// Internally synchronized; safe for N concurrent goroutines (D-025).
// Coalesces concurrent first-time fetches via a per-URL inflight map
// so a stampede of N concurrent Discover calls results in one HTTP GET.
type agentCardCache struct {
	mu       sync.Mutex
	entries  map[string]agentCardEntry
	inflight map[string]chan struct{}
	httpc    *http.Client
	now      func() time.Time
}

// newAgentCardCache constructs a fresh cache. Tests pass a non-default
// `now` for deterministic TTL math.
func newAgentCardCache(httpc *http.Client, now func() time.Time) *agentCardCache {
	if now == nil {
		now = time.Now
	}
	if httpc == nil {
		httpc = http.DefaultClient
	}
	return &agentCardCache{
		entries:  map[string]agentCardEntry{},
		inflight: map[string]chan struct{}{},
		httpc:    httpc,
		now:      now,
	}
}

// Fetch returns the AgentCard for peerBaseURL. If the cache holds a
// fresh entry, returns it without I/O. Otherwise issues a coalesced
// `GET <peerBaseURL>/.well-known/agent-card.json` and stores the
// result with the supplied TTL.
//
// The caller MUST validate the peer URL via validatePeerURL BEFORE
// invoking Fetch — the cache trusts the input scheme.
func (c *agentCardCache) Fetch(ctx context.Context, peerBaseURL string, ttl time.Duration) (*a2a.AgentCard, error) {
	if ttl <= 0 {
		ttl = defaultAgentCardTTL
	}
	for {
		c.mu.Lock()
		if entry, ok := c.entries[peerBaseURL]; ok && c.now().Before(entry.expires) {
			c.mu.Unlock()
			return entry.card, nil
		}
		if waitCh, ok := c.inflight[peerBaseURL]; ok {
			c.mu.Unlock()
			// Another goroutine is fetching; wait for it to finish
			// then re-read the entry (it MAY have failed and we'll
			// retry by looping; success path returns from the
			// freshness check above).
			select {
			case <-waitCh:
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		waitCh := make(chan struct{})
		c.inflight[peerBaseURL] = waitCh
		c.mu.Unlock()

		card, err := c.doFetch(ctx, peerBaseURL)

		c.mu.Lock()
		delete(c.inflight, peerBaseURL)
		close(waitCh)
		if err == nil && card != nil {
			c.entries[peerBaseURL] = agentCardEntry{
				card:    card,
				expires: c.now().Add(ttl),
			}
		}
		c.mu.Unlock()
		return card, err
	}
}

// Invalidate drops the cache entry for peerBaseURL. Called when the
// JSON-RPC layer detects a schema-mismatch error suggesting the peer
// has hot-swapped its card.
func (c *agentCardCache) Invalidate(peerBaseURL string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, peerBaseURL)
}

// doFetch performs the HTTP GET + parse, no locking. Honour ctx.
func (c *agentCardCache) doFetch(ctx context.Context, peerBaseURL string) (*a2a.AgentCard, error) {
	cardURL, err := agentCardURL(peerBaseURL)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cardURL, nil)
	if err != nil {
		return nil, fmt.Errorf("a2a: build agent-card request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "harbor-a2a/1.0")

	resp, err := c.httpc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("a2a: fetch agent-card: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, agentCardMaxBytes))
	if err != nil {
		return nil, fmt.Errorf("a2a: read agent-card body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: status=%d body=%q", ErrAgentCardSchemaInvalid, resp.StatusCode, snippet(body, 256))
	}

	var card a2a.AgentCard
	if err := json.Unmarshal(body, &card); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrAgentCardSchemaInvalid, err)
	}
	// Validate the minimum invariants the spec requires before the
	// card is usable.
	if card.Name == "" || card.Version == "" {
		return nil, fmt.Errorf("%w: missing required Name or Version", ErrAgentCardSchemaInvalid)
	}
	if len(card.SupportedInterfaces) == 0 {
		return nil, fmt.Errorf("%w: AgentCard declares no SupportedInterfaces", ErrAgentCardSchemaInvalid)
	}
	return &card, nil
}

// agentCardURL composes the AgentCard discovery URL for peerBaseURL.
// Per the .well-known convention (IETF), AgentCard lives at the host
// root: `<scheme>://<host>/.well-known/agent-card.json`. The peer's
// path component (when present) is intentionally dropped.
func agentCardURL(peerBaseURL string) (string, error) {
	// Light path-stripping without bringing in net/url to avoid an
	// import cycle with security.go.
	scheme, hostAndPath, ok := splitScheme(peerBaseURL)
	if !ok {
		return "", fmt.Errorf("%w: missing scheme in %q", ErrInvalidPeerURL, peerBaseURL)
	}
	host := hostAndPath
	if i := strings.Index(hostAndPath, "/"); i >= 0 {
		host = hostAndPath[:i]
	}
	if host == "" {
		return "", fmt.Errorf("%w: missing host in %q", ErrInvalidPeerURL, peerBaseURL)
	}
	return scheme + "://" + host + AgentCardPath, nil
}

// splitScheme splits `scheme://rest` into (scheme, rest, true). Returns
// (_, _, false) when `://` is absent.
func splitScheme(raw string) (string, string, bool) {
	const sep = "://"
	i := strings.Index(raw, sep)
	if i < 0 {
		return "", "", false
	}
	return raw[:i], raw[i+len(sep):], true
}

// agentCardMaxBytes caps the response body size for an AgentCard. The
// spec doesn't bound this; a hostile peer that tries to OOM the
// runtime with a 1 GiB card is rejected here. 1 MiB is plenty for a
// realistic card (the largest field is `Description` which would be
// in the low-KB range).
const agentCardMaxBytes int64 = 1 << 20

// firstJSONRPCInterface returns the AgentInterface declaring
// ProtocolBinding == "JSONRPC", or nil when none. Phase 29 only
// implements the JSON-RPC binding.
func firstJSONRPCInterface(card *a2a.AgentCard) *a2a.AgentInterface {
	for i := range card.SupportedInterfaces {
		if card.SupportedInterfaces[i].ProtocolBinding == a2a.ProtocolBindingJSONRPC {
			return &card.SupportedInterfaces[i]
		}
	}
	return nil
}

// snippet returns a UTF-8-safe truncated view of body for error
// messages. Never logs the full body — auditing of remote payloads
// belongs upstream.
func snippet(body []byte, max int) string {
	if len(body) <= max {
		return string(body)
	}
	return string(body[:max]) + "…"
}
