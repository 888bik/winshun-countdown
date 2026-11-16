//go:build windows

package main

import (
	"fmt"
	"syscall"
	"unsafe"
)

const (
	sndAsync    = 0x0001
	sndPurge    = 0x0040
	sndAlias    = 0x00010000
	sndFilename = 0x00020000
)

var (
	winmm          = syscall.NewLazyDLL("winmm.dll")
	procPlaySoundW = winmm.NewProc("PlaySoundW")
)

type winSoundBackend struct{}

func newSoundBackend() soundBackend {
	return winSoundBackend{}
}

func (winSoundBackend) PlayFile(path string) error {
	name, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	result, _, callErr := procPlaySoundW.Call(
		uintptr(unsafe.Pointer(name)),
		0,
		uintptr(sndFilename|sndAsync),
	)
	if result == 0 {
		if callErr != syscall.Errno(0) {
			return fmt.Errorf("PlaySoundW failed: %w", callErr)
		}
		return fmt.Errorf("PlaySoundW failed")
	}
	return nil
}

func (winSoundBackend) PlayDefault() error {
	alias, err := syscall.UTF16PtrFromString("SystemAsterisk")
	if err != nil {
		return err
	}
	result, _, callErr := procPlaySoundW.Call(
		uintptr(unsafe.Pointer(alias)),
		0,
		uintptr(sndAlias|sndAsync),
	)
	if result == 0 {
		if callErr != syscall.Errno(0) {
			return fmt.Errorf("PlaySoundW system alias failed: %w", callErr)
		}
		return fmt.Errorf("PlaySoundW system alias failed")
	}
	return nil
}

func (winSoundBackend) Stop() {
	procPlaySoundW.Call(0, 0, uintptr(sndPurge))
}
