package auth

import "testing"

// wire_injection_gate_test.go — the boot-captured env half of the wire-injection
// opt-in. The default is closed (never captured ⇒ false); a capture flips it; and
// it is INDEPENDENT of the wire-OAuth-descriptor capture (setting one must not
// move the other).

func TestAllowWireInjectionCaptured_DefaultClosedAndToggles(t *testing.T) {
	// Save + restore both process-global captures so the test is isolated.
	origInj := AllowWireInjectionCaptured()
	origOAuth := AllowWireOAuthDescriptorCaptured()
	t.Cleanup(func() {
		RegisterAllowWireInjectionCaptured(origInj)
		RegisterAllowWireOAuthDescriptorCaptured(origOAuth)
	})

	RegisterAllowWireInjectionCaptured(false)
	if AllowWireInjectionCaptured() {
		t.Fatal("captured=false must read closed")
	}
	RegisterAllowWireInjectionCaptured(true)
	if !AllowWireInjectionCaptured() {
		t.Fatal("captured=true must read open")
	}

	// Independence: flipping the wire-OAuth capture must not move the injection one.
	RegisterAllowWireOAuthDescriptorCaptured(false)
	if !AllowWireInjectionCaptured() {
		t.Fatal("wire-injection capture must be independent of the wire-OAuth capture")
	}
}
