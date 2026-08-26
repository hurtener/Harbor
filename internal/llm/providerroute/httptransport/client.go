// Package httptransport resolves explicit provider routes over one boot-pinned
// authenticated endpoint. Construction performs no network I/O and starts no
// goroutines or timers.
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
	"time"
	"unicode"

	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/llm/providerroute"
)

var ErrInvalidConfig = errors.New("llm/providerroute/httptransport: invalid configuration")

type Config struct {
	ResolverURL string
	AuthToken   string
	Timeout     time.Duration
	HTTPClient  *http.Client
}

// Client is immutable and safe for concurrent reuse. It deliberately does not
// cache credential material, so rotation and revocation take effect on the
// next provider attempt.
type Client struct {
	resolverURL string
	authToken   string
	httpClient  *http.Client
}

func New(cfg Config) (*Client, error) {
	u, err := url.Parse(cfg.ResolverURL)
	if strings.IndexFunc(cfg.ResolverURL, unicode.IsControl) >= 0 || err != nil || u.Host == "" || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") ||
		u.User != nil || u.RawQuery != "" || u.Fragment != "" ||
		(u.Scheme == "http" && !isLoopback(u.Hostname())) || strings.TrimSpace(cfg.AuthToken) == "" || cfg.Timeout < 0 {
		return nil, ErrInvalidConfig
	}
	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = 10 * time.Second
	}
	var hc *http.Client
	if cfg.HTTPClient == nil {
		hc = &http.Client{Timeout: timeout}
	} else {
		clone := *cfg.HTTPClient
		clone.Timeout = timeout
		hc = &clone
	}
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return errors.New("provider route redirect refused") }
	return &Client{resolverURL: cfg.ResolverURL, authToken: cfg.AuthToken, httpClient: hc}, nil
}

func (c *Client) ResolveProviderRoute(ctx context.Context, input llm.ProviderRouteRequest) (llm.ResolvedProviderRoute, error) {
	body, err := providerroute.MarshalRequest(input)
	if err != nil {
		return llm.ResolvedProviderRoute{}, err
	}
	raw, err := c.request(ctx, body)
	if err != nil {
		return llm.ResolvedProviderRoute{}, err
	}
	return providerroute.ParseResponse(input, raw)
}

// SelectProviderRoute performs the credential-free pre-policy selection leg.
func (c *Client) SelectProviderRoute(ctx context.Context, input llm.ProviderRouteRequest) (llm.SelectedProviderRoute, error) {
	body, err := providerroute.MarshalSelectionRequest(input)
	if err != nil {
		return llm.SelectedProviderRoute{}, err
	}
	raw, err := c.request(ctx, body)
	if err != nil {
		return llm.SelectedProviderRoute{}, err
	}
	return providerroute.ParseSelectionResponse(input, raw)
}

func (c *Client) request(ctx context.Context, body []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.resolverURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("provider route: build request")
	}
	req.Header.Set("Authorization", "Bearer "+c.authToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("provider route: authenticated request failed")
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, providerroute.MaxResponseBytes+1))
	if err != nil || len(raw) > providerroute.MaxResponseBytes {
		return nil, fmt.Errorf("provider route: response read failed")
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("provider route: resolver returned non-success status")
	}
	return raw, nil
}

func (c *Client) Close() error {
	if c != nil && c.httpClient != nil {
		c.httpClient.CloseIdleConnections()
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
