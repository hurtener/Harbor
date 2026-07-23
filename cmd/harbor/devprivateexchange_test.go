package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/tools/auth/drivers/tokenexchange"
)

// TestRegisterAllowPrivateExchangeIfDev_Banner proves the dev-only private-IP
// token-exchange hatch prints its stderr banner exactly when the escape hatch
// fired, is silent otherwise, and is nil-stderr safe — the banner half of the
// reciprocal capture+banner pair (mirroring registerMockIfDevAllowMock). The
// capture half — that RegisterAllowPrivateExchangeCaptured flips the boot flag
// — is pinned in the tokenexchange package's reciprocity test; here we pin the
// cmd-side surface.
func TestRegisterAllowPrivateExchangeIfDev_Banner(t *testing.T) {
	// The helper flips a package-level boot-capture atomic; restore it.
	t.Cleanup(func() { tokenexchange.RegisterAllowPrivateExchangeCaptured(false) })

	// allow=true → the banner prints.
	var on bytes.Buffer
	registerAllowPrivateExchangeIfDev(true, &on)
	if !strings.Contains(on.String(), PrivateExchangeBanner) {
		t.Fatalf("allow=true: banner %q not printed, got %q", PrivateExchangeBanner, on.String())
	}

	// allow=false → silent (the capture is still called unconditionally with
	// false, the honest non-dev boot state — no banner).
	var off bytes.Buffer
	registerAllowPrivateExchangeIfDev(false, &off)
	if off.Len() != 0 {
		t.Fatalf("allow=false: expected no banner, got %q", off.String())
	}

	// allow=true with a nil stderr must not panic (defensive, like the mock helper).
	registerAllowPrivateExchangeIfDev(true, nil)
}
