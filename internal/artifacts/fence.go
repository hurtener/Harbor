package artifacts

import (
	"context"
	"errors"
)

// ErrScopeFenced rejects a write to an owner scope erased by the runtime.
var ErrScopeFenced = errors.New("artifacts: owner scope is permanently fenced")

// ScopeFencer is an actual optional storage capability: only stores whose
// byte transaction atomically serializes with a permanent owner tombstone may
// implement it. The memory and SQL-blob drivers implement it; FS and S3 do not.
// Once FenceScope returns, every prior Put has committed or rolled back and
// every later Put (including ordinary uploads) fails with ErrScopeFenced.
// Get/GetRef/Exists hide fenced data; List/Delete remain available to cleanup.
// A fence is not removed by deleting artifacts and survives persistent reopen.
type ScopeFencer interface {
	FenceScope(context.Context, ArtifactScope) error
	ScopeFenced(context.Context, ArtifactScope) (bool, error)
}
