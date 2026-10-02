package tasks

// RejectBeforeSpawn records the task registry's proof that a validation refusal
// occurred before task acceptance or any persistence effect. It must never wrap
// a driver error after a task write or an uncertain acceptance. It preserves the
// original error for errors.Is/errors.As and leaves nil unchanged.
func RejectBeforeSpawn(err error) error {
	if err == nil {
		return nil
	}
	return &spawnRejection{cause: err}
}

type spawnRejection struct{ cause error }

func (e *spawnRejection) Error() string { return e.cause.Error() }
func (e *spawnRejection) Unwrap() error { return e.cause }

// IsRejectedBeforeSpawn recognizes explicit registry proof. An error code or
// sentinel by itself is insufficient, and a joined unknown outcome prevents
// treating the whole result as a refusal before acceptance.
func IsRejectedBeforeSpawn(err error) bool {
	switch failure := err.(type) {
	case *spawnRejection:
		return true
	case interface{ Unwrap() []error }:
		causes := failure.Unwrap()
		if len(causes) == 0 {
			return false
		}
		for _, cause := range causes {
			if !IsRejectedBeforeSpawn(cause) {
				return false
			}
		}
		return true
	case interface{ Unwrap() error }:
		return IsRejectedBeforeSpawn(failure.Unwrap())
	default:
		return false
	}
}
