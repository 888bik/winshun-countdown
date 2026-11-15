//go:build !windows

package main

// stubWindow keeps the core testable on Linux; the real Win32 window lives in
// window_windows.go.
type stubWindow struct{}

func newWindowBackend(_ windowCallbacks) windowBackend {
	return stubWindow{}
}

func (stubWindow) Show([]pendingItem) bool { return false }
func (stubWindow) Refresh([]pendingItem)   {}
func (stubWindow) Hide()                   {}
func (stubWindow) Visible() bool           { return false }
func (stubWindow) SetAutoClose(int)        {}
func (stubWindow) SetAnimations(bool)      {}

func initDpiAwareness() {}
