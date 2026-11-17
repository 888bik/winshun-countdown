//go:build !windows

package main

// stubIslandBackend keeps the plugin core testable on non-Windows platforms.
type stubIslandBackend struct{}

func newIslandBackend(_ islandCallbacks) islandBackend      { return stubIslandBackend{} }
func (stubIslandBackend) SetEnabled(bool)                   {}
func (stubIslandBackend) Refresh([]trayTimerSnapshot, bool) {}
func (stubIslandBackend) Hide()                             {}
func (stubIslandBackend) Visible() bool                     { return false }
