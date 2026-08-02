package tokenexchange

import (
	"context"
	"errors"
	"net"
	"net/http"
	"testing"
	"time"
)

// TestIsBlockedDialIP_DecisionMatrix pins the dial-guard decision for both
// the default (armed) and the dev-only opt-in (relaxed) posture. It is the
// single source of the private-dial decision, so a deterministic table here
// is the primary guard — no network is touched.
func TestIsBlockedDialIP_DecisionMatrix(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		ip   string
		// blockedDefault is the decision when allowPrivate is false (armed).
		blockedDefault bool
		// blockedOptIn is the decision when allowPrivate is true (relaxed).
		blockedOptIn bool
	}{
		// Link-local unicast (169.254/16, fe80::/10) — blocked by default,
		// permitted under the opt-in.
		{"link_local_unicast_v4", "169.254.10.20", true, false},
		{"link_local_unicast_v6", "fe80::1", true, false},
		// Link-local multicast — blocked by default, permitted under opt-in.
		{"link_local_multicast_v4", "224.0.0.251", true, false},
		// RFC 1918 private ranges — the containerized-broker case.
		{"private_10", "10.0.0.1", true, false},
		{"private_172", "172.16.5.4", true, false},
		{"private_192", "192.168.1.1", true, false},
		// IPv6 unique-local (ULA, fc00::/7).
		{"ula_v6", "fd00::1", true, false},
		// Unspecified (0.0.0.0 / ::) — ALWAYS blocked, even under the opt-in;
		// never a valid coordinator.
		{"unspecified_v4", "0.0.0.0", true, true},
		{"unspecified_v6", "::", true, true},
		// Loopback — ALWAYS allowed (never listed); the localhost-sidecar
		// carve-out holds regardless of the opt-in.
		{"loopback_v4", "127.0.0.1", false, false},
		{"loopback_v6", "::1", false, false},
		// Ordinary public address — always allowed.
		{"public", "203.0.113.10", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ip := net.ParseIP(tc.ip)
			if ip == nil {
				t.Fatalf("bad test IP %q", tc.ip)
			}
			if got := isBlockedDialIP(ip, false); got != tc.blockedDefault {
				t.Errorf("isBlockedDialIP(%s, allowPrivate=false) = %v, want %v", tc.ip, got, tc.blockedDefault)
			}
			if got := isBlockedDialIP(ip, true); got != tc.blockedOptIn {
				t.Errorf("isBlockedDialIP(%s, allowPrivate=true) = %v, want %v", tc.ip, got, tc.blockedOptIn)
			}
		})
	}
}

// TestHardenTokenExchangeClient_DialControlWiring proves hardenTokenExchangeClient
// threads allowPrivate into the dialer's post-resolution Control hook, both
// ways, against parsed IPs — deterministic, bounded, no successful network I/O.
func TestHardenTokenExchangeClient_DialControlWiring(t *testing.T) {
	t.Parallel()

	dialControl := func(c *http.Client) func(ctx context.Context, network, addr string) (net.Conn, error) {
		tr, ok := c.Transport.(*http.Transport)
		if !ok {
			t.Fatalf("hardened client transport is %T, want *http.Transport", c.Transport)
		}
		return tr.DialContext
	}

	// Default (armed): a private dial is refused at Control time — returns
	// immediately with ErrPrivateDialRefused, before any TCP connect.
	armed, _ := hardenTokenExchangeClient(nil, 5*time.Second, false)
	if _, err := dialControl(armed)(context.Background(), "tcp", "10.255.255.1:9"); !errors.Is(err, ErrPrivateDialRefused) {
		t.Fatalf("armed dial to private IP: want ErrPrivateDialRefused, got %v", err)
	}

	// Opt-in: the SAME private IP passes Control (returns nil); the dial then
	// proceeds and fails on the unreachable host bounded by the dialer
	// timeout — the error is NOT ErrPrivateDialRefused (the guard let it
	// through).
	relaxed, _ := hardenTokenExchangeClient(nil, 50*time.Millisecond, true)
	_, err := dialControl(relaxed)(context.Background(), "tcp", "10.255.255.1:9")
	if err == nil {
		t.Fatal("opt-in dial to an unreachable private IP unexpectedly succeeded")
	}
	if errors.Is(err, ErrPrivateDialRefused) {
		t.Fatalf("opt-in dial: guard still refused the private IP (%v) — allowPrivate not threaded through", err)
	}

	// Opt-in does NOT relax the unspecified-address block: 0.0.0.0 is still
	// refused at Control time even with allowPrivate=true.
	if _, err := dialControl(relaxed)(context.Background(), "tcp", "0.0.0.0:9"); !errors.Is(err, ErrPrivateDialRefused) {
		t.Fatalf("opt-in dial to unspecified 0.0.0.0: want ErrPrivateDialRefused (unspecified is never relaxed), got %v", err)
	}
}

// TestAllowPrivateExchangeCaptured_ReciprocityAndReset pins the boot-capture
// atomic: it defaults false, flips true on RegisterAllowPrivateExchangeCaptured(true),
// and the test-only reset restores false.
func TestAllowPrivateExchangeCaptured_ReciprocityAndReset(t *testing.T) {
	// Not parallel: mutates the package-level boot-capture atomic.
	resetAllowPrivateExchangeCapturedForTesting()
	t.Cleanup(resetAllowPrivateExchangeCapturedForTesting)

	if allowPrivateExchangeCaptured() {
		t.Fatal("captured flag should default false")
	}
	RegisterAllowPrivateExchangeCaptured(true)
	if !allowPrivateExchangeCaptured() {
		t.Fatal("RegisterAllowPrivateExchangeCaptured(true) did not flip the captured flag")
	}
	RegisterAllowPrivateExchangeCaptured(false)
	if allowPrivateExchangeCaptured() {
		t.Fatal("RegisterAllowPrivateExchangeCaptured(false) did not clear the captured flag")
	}
	RegisterAllowPrivateExchangeCaptured(true)
	resetAllowPrivateExchangeCapturedForTesting()
	if allowPrivateExchangeCaptured() {
		t.Fatal("resetAllowPrivateExchangeCapturedForTesting did not restore false")
	}
}
