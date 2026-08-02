package integration

import "testing"

// TestE2E_WaveV126 deliberately shares its public test name with the external
// composition in wave_v126_test.go. Go runs the internal and external test
// packages together; keeping the fidelity leg here lets the checkpoint invoke
// the real D-402 fixture without recreating its hermetic Bifrost provider or
// weakening the durable restart assertion into a nested `go test` invocation.
func TestE2E_WaveV126(t *testing.T) {
	t.Run("reasoning_durable", TestE2E_Phase233c_ReasoningFidelity_DurableRestart)
}
