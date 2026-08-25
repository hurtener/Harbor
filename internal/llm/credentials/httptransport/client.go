// Package httptransport resolves already-verified external-grant credentials
// over one boot-pinned authenticated coordinator endpoint.
package httptransport

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/llm/credentials"
)

const (
	defaultTimeout  = 10 * time.Second
	defaultCacheTTL = 30 * time.Second
	maxCacheEntries = 256
)

var (
	// ErrInvalidConfig identifies an unsafe or incomplete boot transport.
	ErrInvalidConfig = errors.New("llm/credentials/httptransport: invalid configuration")
	// ErrResolution identifies a bounded credential-resolution failure.
	ErrResolution = errors.New("llm/credentials/httptransport: resolution failed")
	// ErrClosed identifies use after the client has been closed.
	ErrClosed = errors.New("llm/credentials/httptransport: client closed")
)

// Config is immutable constructor input. AuthToken is a runtime service
// credential and must never be logged or serialized.
type Config struct {
	CredentialURL string
	AuthToken     string
	Timeout       time.Duration
	HTTPClient    *http.Client
	Clock         func() time.Time
}

type cacheEntry struct {
	credential llm.ResolvedCredential
	expiresAt  time.Time
	insertedAt time.Time
}

// Client is safe for concurrent reuse. Request authority comes exclusively
// from the verified grant context installed by Harbor's grant wrapper.
type Client struct {
	credentialURL string
	authToken     string
	timeout       time.Duration
	httpClient    *http.Client
	clock         func() time.Time

	mu     sync.Mutex
	cache  map[string]cacheEntry
	closed bool
	epoch  uint64
	group  singleflight.Group
}

// New constructs a boot-pinned resolver without network I/O, timers, polls,
// goroutines, or StateStore reads.
func New(cfg Config) (*Client, error) {
	if err := validateEndpoint(cfg.CredentialURL); err != nil {
		return nil, fmt.Errorf("%w: credential endpoint", ErrInvalidConfig)
	}
	if strings.TrimSpace(cfg.AuthToken) == "" {
		return nil, fmt.Errorf("%w: authentication token is empty", ErrInvalidConfig)
	}
	if cfg.Timeout < 0 {
		return nil, fmt.Errorf("%w: timeout is negative", ErrInvalidConfig)
	}
	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = defaultTimeout
	}
	clock := cfg.Clock
	if clock == nil {
		clock = time.Now
	}
	var hc *http.Client
	if cfg.HTTPClient == nil {
		hc = &http.Client{Timeout: timeout}
	} else {
		clone := *cfg.HTTPClient
		clone.Timeout = timeout
		hc = &clone
	}
	hc.CheckRedirect = func(*http.Request, []*http.Request) error {
		return fmt.Errorf("%w: redirect refused", ErrResolution)
	}
	return &Client{
		credentialURL: cfg.CredentialURL,
		authToken:     cfg.AuthToken,
		timeout:       timeout,
		httpClient:    hc,
		clock:         clock,
		cache:         make(map[string]cacheEntry),
	}, nil
}

// Resolve implements llm.CredentialResolver. It refuses a grant that is not
// the exact grant already installed in Harbor's verified driver context.
func (c *Client) Resolve(ctx context.Context, grant llm.ExternalGrant) (llm.ResolvedCredential, error) {
	if err := ctx.Err(); err != nil {
		return llm.ResolvedCredential{}, fmt.Errorf("%w: canceled", ErrResolution)
	}
	verified, ok := llm.VerifiedGrantContextFrom(ctx)
	if !ok {
		return llm.ResolvedCredential{}, fmt.Errorf("%w: verified grant context is absent", ErrResolution)
	}
	key, err := llm.CanonicalExternalGrantHash(grant)
	if err != nil {
		return llm.ResolvedCredential{}, fmt.Errorf("%w: invalid grant", ErrResolution)
	}
	verifiedKey, err := llm.CanonicalExternalGrantHash(verified.Grant)
	if err != nil || verifiedKey != key {
		return llm.ResolvedCredential{}, fmt.Errorf("%w: verified grant mismatch", ErrResolution)
	}
	request, err := credentials.NewRequest(grant)
	if err != nil {
		return llm.ResolvedCredential{}, fmt.Errorf("%w: invalid grant", ErrResolution)
	}
	now := c.clock().UTC()
	if cached, ok := c.cached(key, now); ok {
		return cached, nil
	}

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return llm.ResolvedCredential{}, ErrClosed
	}
	epoch := c.epoch
	c.mu.Unlock()

	result := c.group.DoChan(key, func() (any, error) {
		// One caller's cancellation must not cancel another caller sharing this
		// exact verified binding. The immutable client timeout bounds the fetch.
		fetchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.timeout)
		defer cancel()
		return c.fetch(fetchCtx, key, epoch, request)
	})
	select {
	case <-ctx.Done():
		return llm.ResolvedCredential{}, fmt.Errorf("%w: canceled", ErrResolution)
	case outcome := <-result:
		if outcome.Err != nil {
			return llm.ResolvedCredential{}, outcome.Err
		}
		resolved, ok := outcome.Val.(llm.ResolvedCredential)
		if !ok {
			return llm.ResolvedCredential{}, fmt.Errorf("%w: invalid internal result", ErrResolution)
		}
		return resolved, nil
	}
}

func (c *Client) fetch(ctx context.Context, key string, epoch uint64, request credentials.Request) (llm.ResolvedCredential, error) {
	payload, err := credentials.MarshalCanonicalRequest(request)
	if err != nil {
		return llm.ResolvedCredential{}, fmt.Errorf("%w: canonical request", ErrResolution)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.credentialURL, bytes.NewReader(payload))
	if err != nil {
		return llm.ResolvedCredential{}, fmt.Errorf("%w: build request", ErrResolution)
	}
	req.Header.Set("Authorization", "Bearer "+c.authToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return llm.ResolvedCredential{}, fmt.Errorf("%w: authenticated request failed", ErrResolution)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, credentials.MaxResponseBytes+1))
	if err != nil {
		return llm.ResolvedCredential{}, fmt.Errorf("%w: response read failed", ErrResolution)
	}
	if len(raw) > credentials.MaxResponseBytes {
		return llm.ResolvedCredential{}, fmt.Errorf("%w: response exceeds byte bound", ErrResolution)
	}
	if resp.StatusCode/100 != 2 {
		return llm.ResolvedCredential{}, fmt.Errorf("%w: coordinator returned status %d", ErrResolution, resp.StatusCode)
	}
	parsed, err := credentials.ParseCanonicalResponse(request, raw)
	if err != nil {
		return llm.ResolvedCredential{}, fmt.Errorf("%w: malformed response", ErrResolution)
	}
	now := c.clock().UTC()
	expiresAt := parsed.ExpiresAt.UTC()
	if grantExpiry := request.Grant.ExpiresAt.UTC(); expiresAt.After(grantExpiry) {
		expiresAt = grantExpiry
	}
	if capExpiry := now.Add(defaultCacheTTL); expiresAt.After(capExpiry) {
		expiresAt = capExpiry
	}
	if !expiresAt.After(now) {
		return llm.ResolvedCredential{}, fmt.Errorf("%w: response already expired", ErrResolution)
	}
	resolved := llm.ResolvedCredential{
		Provider:                     parsed.Provider,
		CredentialBindingHandle:      parsed.CredentialBindingHandle,
		CredentialAssetGeneration:    parsed.CredentialAssetGeneration,
		ProviderConnectionGeneration: parsed.ProviderConnectionGeneration,
		Secret:                       parsed.Secret,
	}
	c.storeCached(key, epoch, resolved, now, expiresAt)
	return resolved, nil
}

func (c *Client) cached(key string, now time.Time) (llm.ResolvedCredential, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return llm.ResolvedCredential{}, false
	}
	entry, ok := c.cache[key]
	if !ok {
		return llm.ResolvedCredential{}, false
	}
	if !entry.expiresAt.After(now) {
		delete(c.cache, key)
		return llm.ResolvedCredential{}, false
	}
	return entry.credential, true
}

func (c *Client) storeCached(key string, epoch uint64, resolved llm.ResolvedCredential, now, expiresAt time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.epoch != epoch {
		return
	}
	if len(c.cache) >= maxCacheEntries {
		var oldestKey string
		var oldest time.Time
		for candidate, entry := range c.cache {
			if oldestKey == "" || entry.insertedAt.Before(oldest) {
				oldestKey, oldest = candidate, entry.insertedAt
			}
		}
		delete(c.cache, oldestKey)
	}
	c.cache[key] = cacheEntry{credential: resolved, expiresAt: expiresAt, insertedAt: now}
}

// Close prevents new resolutions and clears all cached credential material.
// In-flight bounded HTTP requests may finish, but their epoch cannot repopulate
// the closed cache.
func (c *Client) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	c.epoch++
	for key := range c.cache {
		delete(c.cache, key)
	}
	c.mu.Unlock()
	c.httpClient.CloseIdleConnections()
	return nil
}

func validateEndpoint(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return ErrInvalidConfig
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return ErrInvalidConfig
	}
	if u.Scheme == "http" && !isLoopback(u.Hostname()) {
		return ErrInvalidConfig
	}
	return nil
}

func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}
