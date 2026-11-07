package mcp

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRefuseEveryCredentialRedirect_RedactsURLQueries(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "https://host/target?access_token=redirect-url-sentinel&type=resource", nil)
	err := refuseEveryCredentialRedirect(request, nil)
	if !errors.Is(err, ErrRedirectToUnlistedHost) {
		t.Fatalf("redirect sentinel lost: %v", err)
	}
	if strings.Contains(err.Error(), "redirect-url-sentinel") || !strings.Contains(err.Error(), "access_token=REDACTED&type=resource") {
		t.Fatalf("redirect diagnostic unsafe: %v", err)
	}
	if got := request.URL.Query().Get("access_token"); got != "redirect-url-sentinel" {
		t.Fatal("redaction mutated request")
	}
}
