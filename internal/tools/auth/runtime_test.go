package auth

import "runtime"

// runtimeNumGoroutine is a thin wrapper used by goroutine-leak tests
// so the test file does not import runtime directly (keeps imports
// short and the helper grep-able).
func runtimeNumGoroutine() int { return runtime.NumGoroutine() }
