package control_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/types"
)

func TestServeHTTP_InputReceipt_DeclinedLookupAndConflict(t *testing.T) {
	h, cleanup := newTestHandler(t)
	defer cleanup()
	started := do(t, h, "/v1/control/start", `{"identity":{"tenant":"t1","user":"u1","session":"s1"},"query":"original"}`)
	if started.Code != http.StatusOK {
		t.Fatalf("start=%d %s", started.Code, started.Body.String())
	}
	var start types.StartResponse
	if err := json.Unmarshal(started.Body.Bytes(), &start); err != nil {
		t.Fatal(err)
	}
	identity := fmt.Sprintf(`{"tenant":"t1","user":"u1","session":"s1","run":%q}`, start.TaskID)
	input := func(text string) string {
		return fmt.Sprintf(`{"identity":%s,"event_id":"stable-key","payload":{"message":%q}}`, identity, text)
	}
	// This task is pending and has no executing inbox: refusal is durable
	// metadata, not an ambiguous 404 or a fabricated successful enqueue.
	refused := do(t, h, "/v1/control/user_message", input("correction"))
	if refused.Code != http.StatusOK {
		t.Fatalf("refusal=%d %s", refused.Code, refused.Body.String())
	}
	var receipt types.ControlResponse
	if err := json.Unmarshal(refused.Body.Bytes(), &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.Accepted || receipt.Receipt == nil || receipt.Receipt.Status != "declined" {
		t.Fatalf("refused=%+v", receipt)
	}
	lookup := do(t, h, "/v1/control/control.receipt", fmt.Sprintf(`{"identity":%s,"event_id":"stable-key"}`, identity))
	if lookup.Code != http.StatusOK {
		t.Fatalf("lookup=%d %s", lookup.Code, lookup.Body.String())
	}
	var got types.ControlReceiptResponse
	if err := json.Unmarshal(lookup.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Receipt != *receipt.Receipt {
		t.Fatalf("lookup changed receipt=%+v want=%+v", got.Receipt, receipt.Receipt)
	}
	conflict := do(t, h, "/v1/control/user_message", input("changed"))
	if conflict.Code != http.StatusConflict {
		t.Fatalf("conflict=%d %s", conflict.Code, conflict.Body.String())
	}
	unknown := do(t, h, "/v1/control/control.receipt", fmt.Sprintf(`{"identity":%s,"event_id":"stable-key","payload":{}}`, identity))
	if unknown.Code != http.StatusBadRequest {
		t.Fatalf("unknown shape=%d %s", unknown.Code, unknown.Body.String())
	}
	foreign := do(t, h, "/v1/control/control.receipt", fmt.Sprintf(`{"identity":{"tenant":"t1","user":"other","session":"s1","run":%q},"event_id":"stable-key"}`, start.TaskID))
	if foreign.Code != http.StatusUnauthorized {
		t.Fatalf("foreign=%d %s", foreign.Code, foreign.Body.String())
	}
	var denied protoerrors.Error
	if err := json.Unmarshal(foreign.Body.Bytes(), &denied); err != nil || denied.Code != protoerrors.CodeIdentityRequired {
		t.Fatalf("body-scope refusal=%+v err=%v", denied, err)
	}
}
