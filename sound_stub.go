//go:build !windows

package main

// stubSoundBackend keeps the core testable on non-Windows platforms. Real
// sound playback is implemented by sound_windows.go.
type stubSoundBackend struct{}

func newSoundBackend() soundBackend {
	return stubSoundBackend{}
}

func (stubSoundBackend) PlayFile(string) error { return nil }
func (stubSoundBackend) PlayDefault() error    { return nil }
func (stubSoundBackend) Stop()                 {}
