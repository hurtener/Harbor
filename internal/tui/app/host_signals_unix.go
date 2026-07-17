//go:build darwin || linux

package app

import (
	"os"
	"os/signal"
	"syscall"

	tea "charm.land/bubbletea/v2"
)

func watchHostSignals(program *tea.Program) func() {
	signals := make(chan os.Signal, 1)
	done := make(chan struct{})
	joined := make(chan struct{})
	signal.Notify(signals, syscall.SIGHUP)
	go func() {
		defer close(joined)
		select {
		case <-signals:
			program.Kill()
		case <-done:
		}
	}()
	return func() { signal.Stop(signals); close(done); <-joined }
}
