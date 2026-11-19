//go:build windows

package main

import (
	"syscall"
	"testing"
)

// TestIslandProcsResolve fails loudly if any island Win32 proc is bound to
// the wrong DLL. A wrong binding is invisible on Linux (the island backend
// is a stub there) and crashes the process with an access violation the
// first time the proc is called on Windows.
func TestIslandProcsResolve(t *testing.T) {
	procs := []struct {
		name string
		proc *syscall.LazyProc
	}{
		{"TrackMouseEvent", islandProcTrackMouseEvent},
		{"RoundRect", islandProcRoundRect},
		{"CreatePen", islandProcCreatePen},
		{"GetCursorPos", islandProcGetCursorPos},
		{"GetWindowRect", islandProcGetWindowRect},
		{"SetWindowPos", islandProcSetWindowPos},
		{"GetSystemMetrics", islandProcGetSystemMetrics},
		{"InvalidateRect", islandProcInvalidateRect},
		{"GetStockObject", islandProcGetStockObject},
		{"RegisterClassExW", islandProcRegisterClassEx},
		{"CreateWindowExW", islandProcCreateWindowEx},
		{"DestroyWindow", islandProcDestroyWindow},
		{"ShowWindow", islandProcShowWindow},
		{"SetTimer", islandProcSetTimer},
		{"KillTimer", islandProcKillTimer},
		{"DefWindowProcW", islandProcDefWindowProc},
		{"GetModuleHandleW", islandProcGetModuleHandle},
		{"FillRect", islandProcFillRect},
		{"CreateSolidBrush", islandProcCreateSolidBrush},
		{"DeleteObject", islandProcDeleteObject},
		{"SelectObject", islandProcSelectObject},
		{"SetTextColor", islandProcSetTextColor},
		{"SetBkMode", islandProcSetBkMode},
		{"DrawTextW", islandProcDrawText},
		{"CreateFontW", islandProcCreateFont},
		{"BeginPaint", islandProcBeginPaint},
		{"EndPaint", islandProcEndPaint},
		{"GetClientRect", islandProcGetClientRect},
		{"PostMessageW", islandProcPostMessage},
		{"GetMessageW", islandProcGetMessage},
		{"TranslateMessage", islandProcTranslateMessage},
		{"DispatchMessageW", islandProcDispatchMessage},
		{"PostQuitMessage", islandProcPostQuitMessage},
		{"GetDpiForWindow", islandProcGetDpiForWindow},
		{"GetDC", islandProcGetDC},
		{"ReleaseDC", islandProcReleaseDC},
		{"GetTextExtentPoint32W", islandProcGetTextExtentPoint32},
		{"UpdateLayeredWindow", islandProcUpdateLayeredWindow},
		{"CreateDIBSection", islandProcCreateDIBSection},
		{"CreateCompatibleDC", islandProcCreateCompatibleDC},
		{"DeleteDC", islandProcDeleteDC},
	}
	if len(procs) != 41 {
		t.Fatalf("island proc list has %d entries, want 41; update the list when procs are added", len(procs))
	}
	for _, p := range procs {
		if err := p.proc.Find(); err != nil {
			t.Fatalf("island proc %s not found: %v", p.name, err)
		}
	}
}
