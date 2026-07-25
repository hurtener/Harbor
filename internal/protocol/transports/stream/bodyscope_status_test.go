package stream

import (
	"testing"

	"github.com/hurtener/Harbor/internal/protocol/transports/control"
)

// TestBodyScopeStatus_AgreesWithTheTransportWideBinding — the streaming
// transport writes its own HTTP status for the body-identity gate's
// refusals, because each page handler owns its error writer. That is a
// second place a code maps to a status, so it is pinned against the
// transport-wide binding the control transport exports and the published
// error reference renders: the two transports cannot answer the same
// refusal with different statuses.
//
// The list is every code the shared gate can return.
func TestBodyScopeStatus_AgreesWithTheTransportWideBinding(t *testing.T) {
	t.Parallel()
	for _, code := range bodyScopeCodes() {
		got := bodyScopeStatus(code)
		want := control.HTTPStatus(code)
		if got != want {
			t.Errorf("code %q maps to %d here and %d in the transport-wide binding; the two must agree",
				code, got, want)
		}
	}
}
