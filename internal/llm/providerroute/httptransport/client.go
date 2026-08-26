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
	"net/netip"
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
	return newWithNetwork(cfg, func(ctx context.Context, host string) ([]net.IP, error) {
		return net.DefaultResolver.LookupIP(ctx, "ip", host)
	}, (&net.Dialer{}).DialContext)
}

// newWithNetwork is intentionally unexported: tests use it to exercise the
// address policy without network access; production always supplies the
// standard resolver and dialer through New.
func newWithNetwork(cfg Config, lookupIP func(context.Context, string) ([]net.IP, error), dialContext func(context.Context, string, string) (net.Conn, error)) (*Client, error) {
	u, err := url.Parse(cfg.ResolverURL)
	if strings.IndexFunc(cfg.ResolverURL, unicode.IsControl) >= 0 || err != nil || u == nil || u.Host == "" || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") ||
		u.User != nil || u.RawQuery != "" || u.Fragment != "" ||
		(u.Scheme == "http" && !isLoopback(u.Hostname())) ||
		strings.TrimSpace(cfg.AuthToken) == "" || cfg.Timeout < 0 || cfg.HTTPClient != nil || lookupIP == nil || dialContext == nil {
		return nil, ErrInvalidConfig
	}
	hostIP := net.ParseIP(strings.Trim(u.Hostname(), "[]"))
	if hostIP != nil && ((unsafeDestination(hostIP) && !(u.Scheme == "http" && hostIP.IsLoopback())) || (u.Scheme == "https" && hostIP.IsLoopback())) {
		return nil, ErrInvalidConfig
	}
	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = 10 * time.Second
	}
	hc := &http.Client{Timeout: timeout}
	transport, ok := hc.Transport.(*http.Transport)
	if hc.Transport == nil {
		transport = http.DefaultTransport.(*http.Transport).Clone()
	} else if !ok {
		return nil, ErrInvalidConfig
	} else {
		transport = transport.Clone()
	}
	transport.Proxy = nil
	transport.DialTLSContext = nil
	transport.DialTLS = nil
	transport.DialContext = safeDialContext(u.Scheme, u.Hostname(), lookupIP, dialContext)
	hc.Transport = transport
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return errors.New("provider route redirect refused") }
	return &Client{resolverURL: cfg.ResolverURL, authToken: cfg.AuthToken, httpClient: hc}, nil
}

func safeDialContext(scheme, hostname string, lookupIP func(context.Context, string) ([]net.IP, error), dialContext func(context.Context, string, string) (net.Conn, error)) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		_, port, splitErr := net.SplitHostPort(addr)
		if splitErr != nil {
			return nil, ErrInvalidConfig
		}
		ips, err := lookupIP(ctx, hostname)
		if err != nil || len(ips) == 0 {
			return nil, ErrInvalidConfig
		}
		for _, ip := range ips {
			blocked := unsafeDestination(ip)
			if scheme == "http" && ip.IsLoopback() {
				blocked = false
			}
			if blocked || (scheme == "http" && !ip.IsLoopback()) || (scheme == "https" && ip.IsLoopback()) {
				return nil, ErrInvalidConfig
			}
		}
		for _, ip := range ips {
			conn, dialErr := dialContext(ctx, network, net.JoinHostPort(ip.String(), port))
			if dialErr == nil {
				return conn, nil
			}
		}
		return nil, ErrInvalidConfig
	}
}

func unsafeDestination(ip net.IP) bool {
	if ip == nil {
		return true
	}
	addr, err := netip.ParseAddr(ip.String())
	if err != nil {
		return true
	}
	for _, prefix := range unsafePrefixes {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

var unsafePrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"), netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"), netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.0.0.0/24"), netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"), netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("198.18.0.0/15"), netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"), netip.MustParsePrefix("224.0.0.0/4"),
	netip.MustParsePrefix("240.0.0.0/4"), netip.MustParsePrefix("::/96"),
	netip.MustParsePrefix("64:ff9b::/96"), netip.MustParsePrefix("100::/64"),
	netip.MustParsePrefix("2001:2::/48"), netip.MustParsePrefix("2001:10::/28"),
	netip.MustParsePrefix("2001:20::/28"), netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("fc00::/7"), netip.MustParsePrefix("fe80::/10"),
	netip.MustParsePrefix("ff00::/8"),
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
