package assemble_test

import (
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/events"
	_ "github.com/hurtener/Harbor/internal/runtime/assemble"
)

// TestAuditEventTypesAreBusInternal pins the assumption behind durable audit
// retention (D-493): with every subsystem's event types registered, each
// audit.* type is a bus-internal notice. Retention only trims internal audit
// rows; a new canonical audit.* type would silently be kept forever, so adding
// one must revisit D-493.
func TestAuditEventTypesAreBusInternal(t *testing.T) {
	for _, typ := range events.EventTypes() {
		if strings.HasPrefix(string(typ), "audit.") && !events.IsBusInternalNotice(typ) {
			t.Errorf("audit type %q is not a bus-internal notice; revisit D-493 audit retention", typ)
		}
	}
}
