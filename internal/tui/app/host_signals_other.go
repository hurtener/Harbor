//go:build !darwin && !linux

package app

import tea "charm.land/bubbletea/v2"

func watchHostSignals(*tea.Program) func() { return func() {} }
