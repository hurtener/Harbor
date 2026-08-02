package agentcfg

import (
	"errors"
	"testing"
)

func TestCanonicalOAuthMCPURL_D401CanonicalBytes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		raw  string
		want string
		sink string
	}{
		{name: "default port path dot and query", raw: "https://B\u00fccher.example./a/%2e/b?x=%7e&x=1+2", want: "https://xn--bcher-kva.example:443/a/b?x=~&x=1+2", sink: "https://xn--bcher-kva.example:443"},
		{name: "ipv6", raw: "https://[2001:0db8:0:0:0:0:0:1]:8443/", want: "https://[2001:db8::1]:8443/", sink: "https://[2001:db8::1]:8443"},
		{name: "explicit empty query", raw: "https://example.test?", want: "https://example.test:443/?", sink: "https://example.test:443"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, sink, err := CanonicalOAuthMCPURL(tt.raw)
			if err != nil {
				t.Fatalf("CanonicalOAuthMCPURL(%q): %v", tt.raw, err)
			}
			if got != tt.want || sink != tt.sink {
				t.Fatalf("got (%q, %q), want (%q, %q)", got, sink, tt.want, tt.sink)
			}
		})
	}
}

func TestCanonicalOAuthMCPURL_RefusesUnsafeForms(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{
		"http://example.test", "https://user@example.test", "https://example.test/#fragment",
		"https://example.test:0443/", "https://[fe80::1%25en0]/", "https://example.test/%zz",
	} {
		if _, _, err := CanonicalOAuthMCPURL(raw); !errors.Is(err, ErrInvalidOAuthMCPURL) {
			t.Errorf("CanonicalOAuthMCPURL(%q) err = %v, want ErrInvalidOAuthMCPURL", raw, err)
		}
	}
}
