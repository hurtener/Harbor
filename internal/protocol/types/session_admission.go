package types

// SessionsSetAdmissionRequest is an authenticated admin's exact epoch transition.
// Enrollment authority is derived exclusively from the verified token.
type SessionsSetAdmissionRequest struct {
	Identity      IdentityScope `json:"identity"`
	ExpectedEpoch uint64        `json:"expected_epoch"`
	Epoch         uint64        `json:"epoch"`
}

// SessionsSetAdmissionResponse records the installed session epoch. It does not
// assert that tasks or controls accepted before enrollment have finished.
type SessionsSetAdmissionResponse struct {
	Epoch           uint64 `json:"epoch"`
	ProtocolVersion string `json:"protocol_version"`
}
