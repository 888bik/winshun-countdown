//go:build !windows

package main

// stubTrayBackend keeps the plugin and its tray menu model testable on
// non-Windows platforms. The real Explorer notification-area icon is owned
// by tray_windows.go.
type stubTrayBackend struct{}

func newTrayBackend(_ trayCallbacks) trayBackend {
	return stubTrayBackend{}
}

func (stubTrayBackend) SetEnabled(bool)            {}
func (stubTrayBackend) Update([]trayTimerSnapshot) {}
func (stubTrayBackend) TaskbarCreated()            {}
func (stubTrayBackend) Close()                     {}
