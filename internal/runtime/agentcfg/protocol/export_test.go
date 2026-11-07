package protocol

import "time"

// SetSignedOAuthMCPReconcilerClockForTest replaces the attachment retry-window
// clock of one reconciler.
func SetSignedOAuthMCPReconcilerClockForTest(r *SignedOAuthMCPReconciler, now func() time.Time) {
	r.attachBackoff.mu.Lock()
	r.attachBackoff.now = now
	r.attachBackoff.mu.Unlock()
}

// SignedOAuthMCPReconcilerSizesForTest reports the live gate and retry-window
// entry counts, so tests can prove both stay bounded.
func SignedOAuthMCPReconcilerSizesForTest(r *SignedOAuthMCPReconciler) (gates, backoff int) {
	return r.gates.len(), r.attachBackoff.len()
}
