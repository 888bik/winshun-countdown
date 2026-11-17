//go:build windows

package main

import (
	"math"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

const (
	islandClassName     = "MeCountdownIslandWindow"
	islandWMCommand     = wmAppCommand + 20
	islandWMMouseHover  = 0x02A1
	islandWMMouseLeave  = 0x02A3
	islandTMEHover      = 0x00000001
	islandTMELeave      = 0x00000002
	islandTimerLeave    = uintptr(1)
	islandTimerPulse    = uintptr(2)
	islandTimerAnim     = uintptr(3)
	islandTimerTick     = uintptr(4)
	islandTimerLeaveRow = uintptr(5)
	islandExNoActivate  = 0x08000000
	islandExLayered     = 0x00080000
	islandWSPopup       = 0x80000000
	islandSWPShowWindow = 0x0040
	islandSWPNoActivate = 0x0010
	islandSWPNoZOrder   = 0x0004
	islandWheelDelta    = 120
	islandBaseWidth     = islandCollapsedWidth
	islandBaseHeight    = 36
	islandRowHeight     = 52
	islandMaxRows       = 6
	islandCollapsedMin  = 160
	islandCollapsedMax  = 300
	islandCollapsedPad  = 12
	islandCollapsedGap  = 8
)

type islandTrackMouseEvent struct {
	cbSize      uint32
	dwFlags     uint32
	hwndTrack   uintptr
	dwHoverTime uint32
}

var (
	islandUser32                         = syscall.NewLazyDLL("user32.dll")
	islandGDI32                          = syscall.NewLazyDLL("gdi32.dll")
	islandProcTrackMouseEvent            = islandUser32.NewProc("TrackMouseEvent")
	islandProcSetWindowRgn               = islandUser32.NewProc("SetWindowRgn")
	islandProcCreateRoundRectRgn         = islandGDI32.NewProc("CreateRoundRectRgn")
	islandProcRoundRect                  = islandGDI32.NewProc("RoundRect")
	islandProcCreatePen                  = islandGDI32.NewProc("CreatePen")
	islandProcGetCursorPos               = islandUser32.NewProc("GetCursorPos")
	islandProcGetWindowRect              = islandUser32.NewProc("GetWindowRect")
	islandProcSetWindowPos               = islandUser32.NewProc("SetWindowPos")
	islandProcGetSystemMetrics           = islandUser32.NewProc("GetSystemMetrics")
	islandProcInvalidateRect             = islandUser32.NewProc("InvalidateRect")
	islandProcGetStockObject             = islandGDI32.NewProc("GetStockObject")
	islandProcSetLayeredWindowAttributes = islandUser32.NewProc("SetLayeredWindowAttributes")
	islandProcRegisterClassEx            = islandUser32.NewProc("RegisterClassExW")
	islandProcCreateWindowEx             = islandUser32.NewProc("CreateWindowExW")
	islandProcDestroyWindow              = islandUser32.NewProc("DestroyWindow")
	islandProcShowWindow                 = islandUser32.NewProc("ShowWindow")
	islandProcSetTimer                   = islandUser32.NewProc("SetTimer")
	islandProcKillTimer                  = islandUser32.NewProc("KillTimer")
	islandProcDefWindowProc              = islandUser32.NewProc("DefWindowProcW")
	islandProcGetModuleHandle            = syscall.NewLazyDLL("kernel32.dll").NewProc("GetModuleHandleW")
	islandProcFillRect                   = islandUser32.NewProc("FillRect")
	islandProcCreateSolidBrush           = islandGDI32.NewProc("CreateSolidBrush")
	islandProcDeleteObject               = islandGDI32.NewProc("DeleteObject")
	islandProcSelectObject               = islandGDI32.NewProc("SelectObject")
	islandProcSetTextColor               = islandGDI32.NewProc("SetTextColor")
	islandProcSetBkMode                  = islandGDI32.NewProc("SetBkMode")
	islandProcDrawText                   = islandUser32.NewProc("DrawTextW")
	islandProcCreateFont                 = islandGDI32.NewProc("CreateFontW")
	islandProcBeginPaint                 = islandUser32.NewProc("BeginPaint")
	islandProcEndPaint                   = islandUser32.NewProc("EndPaint")
	islandProcGetClientRect              = islandUser32.NewProc("GetClientRect")
	islandProcPostMessage                = islandUser32.NewProc("PostMessageW")
	islandProcGetMessage                 = islandUser32.NewProc("GetMessageW")
	islandProcTranslateMessage           = islandUser32.NewProc("TranslateMessage")
	islandProcDispatchMessage            = islandUser32.NewProc("DispatchMessageW")
	islandProcPostQuitMessage            = islandUser32.NewProc("PostQuitMessage")
	islandProcGetDpiForWindow            = islandUser32.NewProc("GetDpiForWindow")
	islandProcGetDC                      = islandUser32.NewProc("GetDC")
	islandProcReleaseDC                  = islandUser32.NewProc("ReleaseDC")
	islandProcGetTextExtentPoint32       = islandGDI32.NewProc("GetTextExtentPoint32W")
	islandClassMu                        sync.Mutex
	islandClassRegistered                bool
	activeIsland                         *islandWinBackend
)

type islandCommand struct {
	kind    int
	enabled bool
	items   []trayTimerSnapshot
	pending bool
}

const (
	islandCmdEnable = iota + 1
	islandCmdRefresh
	islandCmdHide
)

type islandWinBackend struct {
	callbacks        islandCallbacks
	ready            chan bool
	cmdMu            sync.Mutex
	commands         []islandCommand
	hwnd             uintptr
	enabled          bool
	visible          atomic.Bool
	expanded         bool
	hasPending       bool
	items            []trayTimerSnapshot
	scroll           int
	dpi              int
	width            int
	height           int
	font             uintptr
	mouseTracking    bool
	hoverRow         int
	hoverHalf        int
	leavingID        string
	leavingAction    string
	leaveStart       time.Time
	leaveProgress    float64
	collapsedTitle   string
	collapsedText    string
	collapsedExtra   int
	collapsedPending bool
	anim             animation
	animating        bool
	alpha            int
	corner           int
	animFromW        int
	animFromH        int
	animFromC        int
	animToW          int
	animToH          int
	animToC          int
	failed           bool
}

type islandMessage struct {
	hwnd     uintptr
	message  uint32
	wParam   uintptr
	lParam   uintptr
	time     uint32
	pt       point
	lPrivate uint32
}

func newIslandBackend(callbacks islandCallbacks) islandBackend {
	b := &islandWinBackend{callbacks: callbacks, ready: make(chan bool, 1), dpi: 96, width: islandBaseWidth, height: islandBaseHeight, hoverRow: -1, hoverHalf: -1}
	go b.loop()
	<-b.ready
	return b
}

func (b *islandWinBackend) post(command islandCommand) {
	b.cmdMu.Lock()
	b.commands = append(b.commands, command)
	hwnd := b.hwnd
	b.cmdMu.Unlock()
	if hwnd != 0 {
		islandProcPostMessage.Call(hwnd, uintptr(islandWMCommand), 0, 0)
	}
}

func (b *islandWinBackend) SetEnabled(enabled bool) {
	b.post(islandCommand{kind: islandCmdEnable, enabled: enabled})
}

func (b *islandWinBackend) Refresh(items []trayTimerSnapshot, hasPending bool) {
	b.post(islandCommand{kind: islandCmdRefresh, items: append([]trayTimerSnapshot(nil), items...), pending: hasPending})
}

func (b *islandWinBackend) Hide() { b.post(islandCommand{kind: islandCmdHide}) }

func (b *islandWinBackend) Visible() bool { return b.visible.Load() }

func (b *islandWinBackend) drain() {
	b.cmdMu.Lock()
	commands := b.commands
	b.commands = nil
	b.cmdMu.Unlock()
	for _, command := range commands {
		switch command.kind {
		case islandCmdEnable:
			b.enabled = command.enabled
			if !b.enabled {
				b.hideOnGUI()
			} else {
				b.updateVisibility()
			}
		case islandCmdRefresh:
			// If a row is mid-leave, settle it before replacing the snapshot.
			// This guarantees exactly-once even when a refresh removes it.
			b.finishLeave()
			b.items = command.items
			b.hasPending = command.pending
			b.updateVisibility()
		case islandCmdHide:
			b.hideOnGUI()
		}
	}
}

func (b *islandWinBackend) loop() {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	activeIsland = b
	if !b.ensureWindow() {
		b.ready <- false
		if b.callbacks.Failed != nil {
			b.callbacks.Failed()
		}
		return
	}
	b.ready <- true
	var message islandMessage
	for {
		result, _, _ := islandProcGetMessage.Call(uintptr(unsafe.Pointer(&message)), 0, 0, 0)
		if int32(result) <= 0 {
			break
		}
		islandProcTranslateMessage.Call(uintptr(unsafe.Pointer(&message)))
		islandProcDispatchMessage.Call(uintptr(unsafe.Pointer(&message)))
	}
	if b.font != 0 {
		islandProcDeleteObject.Call(b.font)
		b.font = 0
	}
	// Window teardown is the final safety net for a leave animation.
	b.finishLeave()
	if b.hwnd != 0 {
		islandProcDestroyWindow.Call(b.hwnd)
		b.hwnd = 0
	}
	activeIsland = nil
}

func (b *islandWinBackend) ensureWindow() bool {
	islandClassMu.Lock()
	defer islandClassMu.Unlock()
	if !islandClassRegistered {
		name, _ := syscall.UTF16PtrFromString(islandClassName)
		instance, _, _ := islandProcGetModuleHandle.Call(0)
		class := wndClassEx{cbSize: uint32(unsafe.Sizeof(wndClassEx{})), lpfnWndProc: syscall.NewCallback(islandWndProc), hInstance: instance, lpszClassName: name}
		result, _, _ := islandProcRegisterClassEx.Call(uintptr(unsafe.Pointer(&class)))
		if result == 0 {
			return false
		}
		islandClassRegistered = true
	}
	name, _ := syscall.UTF16PtrFromString(islandClassName)
	title, _ := syscall.UTF16PtrFromString("倒计时灵动岛")
	instance, _, _ := islandProcGetModuleHandle.Call(0)
	hwnd, _, _ := islandProcCreateWindowEx.Call(
		uintptr(wsExTool|wsExTopmost|islandExLayered|islandExNoActivate),
		uintptr(unsafe.Pointer(name)), uintptr(unsafe.Pointer(title)),
		uintptr(islandWSPopup), 0, 0, 1, 1, 0, 0, instance, 0,
	)
	if hwnd == 0 {
		return false
	}
	b.hwnd = hwnd
	activeIsland = b
	b.dpi = b.getDPI()
	// A rounded native region keeps the transparent corners click-through. The
	// layered alpha is uniform; all drawing remains on the GUI thread.
	if result, _, _ := islandProcSetLayeredWindowAttributes.Call(hwnd, 0, 255, 0x2); result == 0 {
		return false
	}
	return true
}

func islandWndProc(hwnd uintptr, message, wParam, lParam uintptr) uintptr {
	if activeIsland != nil && activeIsland.hwnd == hwnd {
		switch message {
		case wmPaint:
			activeIsland.paint()
			return 0
		case islandWMCommand:
			activeIsland.drain()
			return 0
		case wmMouseMove:
			activeIsland.trackMouse()
			activeIsland.updateHover(int(int16(lParam)), int(int16(lParam>>16)))
		case islandWMMouseHover:
			// TME_HOVER is one-shot; re-arm it after expanding so a later
			// move-in still opens the panel.
			activeIsland.mouseTracking = false
			activeIsland.trackMouse()
			// The re-arm above makes WM_MOUSEHOVER fire again after 1ms of
			// stillness, so only expand once: restarting the enter animation
			// on every hover event repainted the panel non-stop and the
			// hover highlight visibly flickered.
			if !activeIsland.expanded {
				activeIsland.expanded = true
				activeIsland.startIslandAnimation(animEnter)
			}
			return 0
		case islandWMMouseLeave:
			activeIsland.mouseTracking = false
			activeIsland.clearHover()
			islandProcSetTimer.Call(hwnd, islandTimerLeave, 300, 0)
			return 0
		case wmTimer:
			switch wParam {
			case islandTimerLeave:
				islandProcKillTimer.Call(hwnd, islandTimerLeave)
				if !activeIsland.pointerInside() {
					activeIsland.clearHover()
					activeIsland.expanded = false
					activeIsland.startIslandAnimation(animExit)
				}
			case islandTimerPulse:
				// Warm orange breathing pulse while at least one timer awaits acknowledgement.
				phase := float64(time.Now().UnixNano()%int64(1400*time.Millisecond)) / float64(1400*time.Millisecond)
				alpha := byte(245 + int(10*(0.5+0.5*math.Sin(phase*2*math.Pi))))
				islandProcSetLayeredWindowAttributes.Call(hwnd, 0, uintptr(alpha), 0x2)
			case islandTimerAnim:
				progress, done := activeIsland.anim.progress(time.Now())
				alpha := byte(237 + int(18*progress))
				if activeIsland.anim.kind == animExit {
					alpha = byte(255 - int(18*progress))
				}
				islandProcSetLayeredWindowAttributes.Call(hwnd, 0, uintptr(alpha), 0x2)
				activeIsland.applyAnimFrame(progress)
				if done {
					islandProcKillTimer.Call(hwnd, islandTimerAnim)
					activeIsland.animating = false
					activeIsland.alpha = 255
					islandProcSetLayeredWindowAttributes.Call(hwnd, 0, 255, 0x2)
					activeIsland.layout()
				}
			case islandTimerLeaveRow:
				activeIsland.stepLeave()
			case islandTimerTick:
				islandProcInvalidateRect.Call(hwnd, 0, 1)
			}
			return 0
		case wmDpiChanged:
			activeIsland.dpi = activeIsland.getDPI()
			activeIsland.layout()
			return 0
		case wmMouseWheel:
			delta := int16(uint16(wParam >> 16))
			maxScroll := len(activeIsland.visibleItems()) - islandMaxRows
			if maxScroll < 0 {
				maxScroll = 0
			}
			if delta < 0 && activeIsland.scroll < maxScroll {
				activeIsland.scroll++
			} else if delta > 0 && activeIsland.scroll > 0 {
				activeIsland.scroll--
			}
			activeIsland.layout()
			return 0
		case wmLButtonUp:
			activeIsland.handleClick(int(int16(lParam)), int(int16(lParam>>16)))
			return 0
		case wmDestroy:
			activeIsland.finishLeave()
		}
	}
	result, _, _ := islandProcDefWindowProc.Call(hwnd, uintptr(message), wParam, lParam)
	return result
}

func (b *islandWinBackend) getDPI() int {
	if b.hwnd != 0 {
		dpi, _, _ := islandProcGetDpiForWindow.Call(b.hwnd)
		if dpi > 0 {
			return int(dpi)
		}
	}
	return 96
}

func (b *islandWinBackend) scale(value int) int {
	dpi := b.dpi
	if dpi <= 0 {
		dpi = 96
	}
	return value * dpi / 96
}

func (b *islandWinBackend) startIslandAnimation(kind int) {
	if b.hwnd == 0 {
		return
	}
	// A new animation starts from the current size, so reversing mid-flight
	// (hover back in during collapse) stays smooth instead of jumping.
	b.cancelIslandAnimation()
	toW, toH, toC := b.islandTargetSize()
	fromC := b.corner
	if fromC == 0 {
		fromC = b.scale(36)
	}
	b.animFromW, b.animFromH, b.animFromC = b.width, b.height, fromC
	b.animToW, b.animToH, b.animToC = toW, toH, toC
	duration := animationEnterDuration
	if kind == animExit {
		duration = animationExitDuration
	}
	b.anim.begin(kind, time.Now(), duration, 0, 1)
	b.animating = true
	islandProcSetTimer.Call(b.hwnd, 3, animationFrameInterval, 0)
}

func (b *islandWinBackend) cancelIslandAnimation() {
	if b.hwnd != 0 && b.animating {
		islandProcKillTimer.Call(b.hwnd, 3)
	}
	b.animating = false
	b.anim.stop()
}

func (b *islandWinBackend) applyAnimFrame(progress float64) {
	w := b.animFromW + int(float64(b.animToW-b.animFromW)*progress)
	h := b.animFromH + int(float64(b.animToH-b.animFromH)*progress)
	c := b.animFromC + int(float64(b.animToC-b.animFromC)*progress)
	b.applyIslandSize(w, h, c)
}

func (b *islandWinBackend) updateVisibility() {
	if b.failed || !b.enabled {
		b.hideOnGUI()
		return
	}
	model := buildIslandViewModel(b.items, b.hasPending, true, b.expanded, time.Now())
	if !model.Visible {
		b.hideOnGUI()
		return
	}
	b.visible.Store(true)
	if b.hasPending {
		islandProcSetTimer.Call(b.hwnd, 2, 80, 0)
	} else {
		islandProcKillTimer.Call(b.hwnd, 2)
		b.alpha = 255
		islandProcSetLayeredWindowAttributes.Call(b.hwnd, 0, 255, 0x2)
	}
	b.layout()
	// One-second tick: the collapsed text and the expanded rows show the
	// remaining time computed at paint time, so invalidating once a second
	// makes the island count down live instead of only on scheduler wakes.
	islandProcSetTimer.Call(b.hwnd, islandTimerTick, 1000, 0)
}

func (b *islandWinBackend) hideOnGUI() {
	// A pending row's action must not be lost just because the panel is
	// collapsing/hiding mid-animation. Settle it before hiding.
	b.finishLeave()
	b.clearHover()
	b.visible.Store(false)
	b.expanded = false
	b.scroll = 0
	if b.hwnd != 0 {
		islandProcKillTimer.Call(b.hwnd, islandTimerLeave)
		islandProcKillTimer.Call(b.hwnd, islandTimerPulse)
		islandProcKillTimer.Call(b.hwnd, islandTimerAnim)
		islandProcKillTimer.Call(b.hwnd, islandTimerTick)
		islandProcKillTimer.Call(b.hwnd, islandTimerLeaveRow)
		islandProcShowWindow.Call(b.hwnd, swHide)
	}
}

type islandTextSize struct {
	cx int32
	cy int32
}

func (b *islandWinBackend) textExtent(hdc uintptr, text string) int {
	if hdc == 0 || text == "" {
		return 0
	}
	old := uintptr(0)
	if b.font != 0 {
		old, _, _ = islandProcSelectObject.Call(hdc, b.font)
	}
	wide, err := syscall.UTF16FromString(text)
	if err != nil {
		if old != 0 {
			islandProcSelectObject.Call(hdc, old)
		}
		return 0
	}
	var size islandTextSize
	islandProcGetTextExtentPoint32.Call(hdc, uintptr(unsafe.Pointer(&wide[0])), uintptr(len(wide)-1), uintptr(unsafe.Pointer(&size)))
	if old != 0 {
		islandProcSelectObject.Call(hdc, old)
	}
	return int(size.cx)
}

func (b *islandWinBackend) collapsedWidth() int {
	model := buildIslandViewModel(b.items, b.hasPending, true, false, time.Now())
	b.collapsedTitle = model.CollapsedTitle
	b.collapsedExtra = model.CollapsedExtra
	b.collapsedText = model.CollapsedText
	b.collapsedPending = model.HasPending
	if b.hwnd == 0 {
		return b.scale(islandCollapsedWidth)
	}
	hdc, _, _ := islandProcGetDC.Call(b.hwnd)
	if hdc == 0 {
		return b.scale(islandCollapsedWidth)
	}
	defer islandProcReleaseDC.Call(b.hwnd, hdc)

	title := model.CollapsedTitle
	text := model.CollapsedText
	extra := ""
	if model.CollapsedExtra > 0 {
		extra = "+" + strconv.Itoa(model.CollapsedExtra)
	}
	measure := func(candidate string) int {
		width := 2 * b.scale(islandCollapsedPad)
		if candidate != "" {
			width += b.textExtent(hdc, candidate)
		}
		if text != "" {
			if candidate != "" {
				width += b.scale(islandCollapsedGap)
			}
			width += b.textExtent(hdc, text)
		}
		if extra != "" {
			width += b.scale(islandCollapsedGap) + b.textExtent(hdc, extra)
		}
		return width
	}
	width := measure(title)
	limit := b.scale(islandCollapsedMax)
	for width > limit && title != "" {
		runes := []rune(title)
		if len(runes) > 1 {
			title = string(runes[:len(runes)-1])
			if title != "" && !strings.HasSuffix(title, "…") {
				title += "…"
			}
		} else {
			title = ""
		}
		width = measure(title)
	}
	b.collapsedTitle = title
	minimum := b.scale(islandCollapsedMin)
	if width < minimum {
		width = minimum
	}
	if width > limit {
		width = limit
	}
	return width
}

// islandTargetSize returns the window size and corner ellipse for the current

// expanded state, in scaled pixels. The size animation interpolates between
// two such targets.
func (b *islandWinBackend) islandTargetSize() (width, height, corner int) {
	if !b.expanded {
		return b.collapsedWidth(), b.scale(islandBaseHeight), b.scale(36)
	}
	width, height = islandExpandedWidth, islandBaseHeight
	corner = 36
	if b.expanded {
		width = islandExpandedWidth
		rows := len(b.visibleItems())
		if rows > islandMaxRows {
			rows = islandMaxRows
		}
		height = 58 + rows*islandRowHeight + 42
		if b.leavingVisibleIndex() >= 0 {
			height -= int(float64(islandRowHeight) * b.leaveProgress)
		}
		corner = 56
	}
	return b.scale(width), b.scale(height), b.scale(corner)
}

func (b *islandWinBackend) islandModelVisible() bool {
	return buildIslandViewModel(b.items, b.hasPending, true, b.expanded, time.Now()).Visible
}

func (b *islandWinBackend) applyIslandSize(width, height, corner int) {
	if b.hwnd == 0 {
		return
	}
	// A late animation frame or 300ms leave timer can arrive after the last
	// pending item was confirmed. Never resurrect an empty island.
	if !b.islandModelVisible() {
		b.hideOnGUI()
		return
	}
	b.width, b.height, b.corner = width, height, corner
	screenW, _, _ := islandProcGetSystemMetrics.Call(0)
	x := (int(screenW) - b.width) / 2
	y := 0
	region, _, _ := islandProcCreateRoundRectRgn.Call(0, 0, uintptr(b.width+1), uintptr(b.height+1), uintptr(corner), uintptr(corner))
	if region != 0 {
		if result, _, _ := islandProcSetWindowRgn.Call(b.hwnd, region, 1); result == 0 {
			islandProcDeleteObject.Call(region)
			b.failOnGUI()
			return
		}
	} else {
		b.failOnGUI()
		return
	}
	if result, _, _ := islandProcSetWindowPos.Call(b.hwnd, 0, uintptr(x), uintptr(y), uintptr(b.width), uintptr(b.height), islandSWPNoActivate|islandSWPNoZOrder|islandSWPShowWindow); result == 0 {
		b.failOnGUI()
		return
	}
	islandProcInvalidateRect.Call(b.hwnd, 0, 1)
	islandProcShowWindow.Call(b.hwnd, swShowNoActivate)
	b.visible.Store(true)
}

func (b *islandWinBackend) layout() {
	if b.hwnd == 0 {
		return
	}
	// A snap (refresh, DPI change, scroll) wins over any in-flight animation.
	b.cancelIslandAnimation()
	// Font first: collapsed width is measured with the real title font.
	if b.font != 0 {
		islandProcDeleteObject.Call(b.font)
	}
	b.font, _, _ = islandProcCreateFont.Call(uintptr(-b.scale(16)), 0, 0, 0, 600, 0, 0, 0, 1, 0, 0, 5, 0, uintptr(unsafe.Pointer(mustUTF16("Segoe UI"))))
	w, h, corner := b.islandTargetSize()
	b.applyIslandSize(w, h, corner)
	if b.failed {
		return
	}
}

func (b *islandWinBackend) visibleItems() []trayTimerSnapshot {
	running, paused, pending := sortedTraySnapshots(b.items)
	out := make([]trayTimerSnapshot, 0, len(b.items))
	out = append(out, running...)
	out = append(out, paused...)
	out = append(out, pending...)
	return out
}

func (b *islandWinBackend) trackMouse() {
	if b.mouseTracking || b.hwnd == 0 {
		return
	}
	event := islandTrackMouseEvent{cbSize: uint32(unsafe.Sizeof(islandTrackMouseEvent{})), dwFlags: islandTMEHover | islandTMELeave, hwndTrack: b.hwnd, dwHoverTime: 1}
	if ok, _, _ := islandProcTrackMouseEvent.Call(uintptr(unsafe.Pointer(&event))); ok != 0 {
		b.mouseTracking = true
	}
}

func (b *islandWinBackend) pointerInside() bool {
	var cursor point
	var bounds rect
	if ok, _, _ := islandProcGetCursorPos.Call(uintptr(unsafe.Pointer(&cursor))); ok == 0 {
		return false
	}
	if ok, _, _ := islandProcGetWindowRect.Call(b.hwnd, uintptr(unsafe.Pointer(&bounds))); ok == 0 {
		return false
	}
	return cursor.x >= bounds.left && cursor.x < bounds.right && cursor.y >= bounds.top && cursor.y < bounds.bottom
}

func (b *islandWinBackend) clearHover() {
	if b.hoverRow == -1 && b.hoverHalf == -1 {
		return
	}
	b.hoverRow, b.hoverHalf = -1, -1
	if b.hwnd != 0 {
		islandProcInvalidateRect.Call(b.hwnd, 0, 0)
	}
}

func (b *islandWinBackend) updateHover(x, y int) {
	if !b.expanded || b.leavingID != "" {
		b.clearHover()
		return
	}
	row, half := islandHoverTarget(
		x, y, b.scale(58), b.scale(islandRowHeight), len(b.visibleItems()),
		b.scale(islandActionStartX), b.scale(islandActionSplitX),
	)
	if row == b.hoverRow && half == b.hoverHalf {
		return
	}
	b.hoverRow, b.hoverHalf = row, half
	if b.hwnd != 0 {
		islandProcInvalidateRect.Call(b.hwnd, 0, 0)
	}
}

func (b *islandWinBackend) startLeave(id, action string) {
	if b.leavingID != "" || b.hwnd == 0 {
		return
	}
	b.leavingID = id
	b.leavingAction = action
	b.leaveStart = time.Now()
	b.leaveProgress = 0
	b.clearHover()
	islandProcSetTimer.Call(b.hwnd, islandTimerLeaveRow, 16, 0)
}

func (b *islandWinBackend) stepLeave() {
	if b.leavingID == "" || b.hwnd == 0 {
		islandProcKillTimer.Call(b.hwnd, islandTimerLeaveRow)
		return
	}
	progress, done := islandLeaveFraction(b.leaveStart, time.Now())
	b.leaveProgress = progress
	if !b.islandModelVisible() {
		b.hideOnGUI()
		return
	}
	islandProcInvalidateRect.Call(b.hwnd, 0, 0)
	w, h, c := b.islandTargetSize()
	b.applyIslandSize(w, h, c)
	if done {
		b.finishLeave()
	}
}

func (b *islandWinBackend) finishLeave() {
	if b.leavingID == "" {
		return
	}
	id, action := b.leavingID, b.leavingAction
	b.leavingID, b.leavingAction = "", ""
	b.leaveProgress = 0
	if b.hwnd != 0 {
		islandProcKillTimer.Call(b.hwnd, islandTimerLeaveRow)
	}
	// Clear our state before invoking: the callback may synchronously
	// Refresh/Hide, and must not be able to settle this action twice.
	if action == "acknowledge" {
		// Drop the row from the local snapshot immediately so a late
		// animation frame cannot paint an empty expanded panel before the
		// asynchronous Refresh arrives.
		filtered := b.items[:0]
		for _, item := range b.items {
			if item.ID != id {
				filtered = append(filtered, item)
			}
		}
		b.items = filtered
		b.hasPending = false
		for _, item := range b.items {
			if normalizedTrayStatus(item.Status) == statusPending {
				b.hasPending = true
				break
			}
		}
	}
	b.invoke(id, action)
	b.updateVisibility()
}

func (b *islandWinBackend) leavingVisibleIndex() int {
	if b.leavingID == "" {
		return -1
	}
	items := b.visibleItems()
	page := pageIndices(len(items), b.scroll, islandMaxRows)
	for visibleIndex, itemIndex := range page {
		if items[itemIndex].ID == b.leavingID {
			return visibleIndex
		}
	}
	return -1
}

func (b *islandWinBackend) handleClick(x, y int) {
	if !b.expanded {
		b.expanded = true
		b.startIslandAnimation(animEnter)
		return
	}
	rowHeight := b.scale(islandRowHeight)
	startY := b.scale(58)
	items := b.visibleItems()
	page := pageIndices(len(items), b.scroll, islandMaxRows)
	leavingIndex := b.leavingVisibleIndex()
	shift := int(float64(rowHeight) * b.leaveProgress)
	for visibleIndex, itemIndex := range page {
		if visibleIndex == leavingIndex {
			return
		}
		top := startY + visibleIndex*rowHeight
		if leavingIndex >= 0 && visibleIndex > leavingIndex {
			top -= shift
		}
		if y < top || y >= top+rowHeight {
			continue
		}
		item := items[itemIndex]
		first, second := islandRowActions(item.Status)
		actionStart := b.scale(islandActionStartX)
		actionSplit := b.scale(islandActionSplitX)
		if x < actionStart {
			return
		}
		action := first
		if x >= actionSplit {
			action = second
		}
		status := normalizedTrayStatus(item.Status)
		if status == statusPending && (action == "acknowledge" || action == "snooze") {
			// Another pending row may already be leaving. Settle that one
			// first so a click on a different row is never swallowed.
			if b.leavingID != "" && b.leavingID != item.ID {
				b.finishLeave()
			}
			b.startLeave(item.ID, action)
			return
		}
		b.invoke(item.ID, action)
		return
	}
	if y >= b.height-b.scale(40) {
		if x < b.width/2 {
			if b.callbacks.OpenList != nil {
				b.callbacks.OpenList()
			}
		} else if b.hasPending && b.callbacks.AckAll != nil {
			b.callbacks.AckAll()
		}
	}
}

func (b *islandWinBackend) invoke(id, action string) {
	dispatchIslandAction(b.callbacks, id, action)
}

func (b *islandWinBackend) failOnGUI() {
	if b.failed {
		return
	}
	b.failed = true
	b.hideOnGUI()
	if b.callbacks.Failed != nil {
		b.callbacks.Failed()
	}
}

func (b *islandWinBackend) paint() {
	if b.hwnd == 0 {
		return
	}
	var ps paintStruct
	hdc, _, _ := islandProcBeginPaint.Call(b.hwnd, uintptr(unsafe.Pointer(&ps)))
	if hdc == 0 {
		return
	}
	defer islandProcEndPaint.Call(b.hwnd, uintptr(unsafe.Pointer(&ps)))
	var client rect
	islandProcGetClientRect.Call(b.hwnd, uintptr(unsafe.Pointer(&client)))
	// The pill stays near-black in every state; an expired timer is signalled
	// by orange text plus the alpha pulse, not a full orange fill.
	brush, _, _ := islandProcCreateSolidBrush.Call(uintptr(0x00141414))
	if brush != 0 {
		islandProcFillRect.Call(hdc, uintptr(unsafe.Pointer(&client)), brush)
		islandProcDeleteObject.Call(brush)
	}
	// Subtle 1px outline so the pill reads against dark wallpapers.
	corner := b.scale(36)
	if b.expanded {
		corner = b.scale(56)
	}
	if pen, _, _ := islandProcCreatePen.Call(0, 1, uintptr(0x00353535)); pen != 0 {
		nullBrush, _, _ := islandProcGetStockObject.Call(5)
		oldPen, _, _ := islandProcSelectObject.Call(hdc, pen)
		var oldBrush uintptr
		if nullBrush != 0 {
			oldBrush, _, _ = islandProcSelectObject.Call(hdc, nullBrush)
		}
		islandProcRoundRect.Call(hdc, 0, 0, uintptr(client.right), uintptr(client.bottom), uintptr(corner), uintptr(corner))
		if nullBrush != 0 {
			islandProcSelectObject.Call(hdc, oldBrush)
		}
		islandProcSelectObject.Call(hdc, oldPen)
		islandProcDeleteObject.Call(pen)
	}
	oldFont := uintptr(0)
	if b.font != 0 {
		oldFont, _, _ = islandProcSelectObject.Call(hdc, b.font)
	}
	islandProcSetBkMode.Call(hdc, 1)
	white := uintptr(0x00FFFFFF)
	orange := uintptr(0x00008CFF)
	islandProcSetTextColor.Call(hdc, white)
	if !b.expanded {
		model := buildIslandViewModel(b.items, b.hasPending, true, false, time.Now())
		title := b.collapsedTitle
		if title == "" {
			title = model.CollapsedTitle
		}
		text := model.CollapsedText
		extra := ""
		if model.CollapsedExtra > 0 {
			extra = "+" + strconv.Itoa(model.CollapsedExtra)
		}
		pad := int32(scaleInt(b.dpi, islandCollapsedPad))
		gap := int32(scaleInt(b.dpi, islandCollapsedGap))
		center := client.right / 2
		if model.HasPending {
			islandProcSetTextColor.Call(hdc, orange)
		}
		drawIslandText(hdc, text, rect{left: 0, top: 0, right: client.right, bottom: client.bottom}, dtCenter|dtVCenter|dtSingleLine)
		islandProcSetTextColor.Call(hdc, white)
		drawIslandText(hdc, title, rect{left: pad, top: 0, right: center - gap, bottom: client.bottom}, dtLeft|dtVCenter|dtSingleLine|dtEndEllipsis)
		if extra != "" {
			if model.HasPending {
				islandProcSetTextColor.Call(hdc, orange)
			} else {
				islandProcSetTextColor.Call(hdc, uintptr(0x00AAAAAA))
			}
			drawIslandText(hdc, extra, rect{left: center + gap, top: 0, right: client.right - pad, bottom: client.bottom}, dtRight|dtVCenter|dtSingleLine)
		}
	} else {
		drawIslandText(hdc, "倒计时", rect{left: 0, top: int32(scaleInt(b.dpi, 8)), right: client.right, bottom: int32(scaleInt(b.dpi, 42))}, dtCenter|dtVCenter|dtSingleLine)
		if divBrush, _, _ := islandProcCreateSolidBrush.Call(uintptr(0x002A2A2A)); divBrush != 0 {
			divRect := rect{left: int32(scaleInt(b.dpi, 12)), top: int32(scaleInt(b.dpi, 48)), right: client.right - int32(scaleInt(b.dpi, 12)), bottom: int32(scaleInt(b.dpi, 48)) + 1}
			islandProcFillRect.Call(hdc, uintptr(unsafe.Pointer(&divRect)), divBrush)
			islandProcDeleteObject.Call(divBrush)
		}
		items := b.visibleItems()
		leavingIndex := b.leavingVisibleIndex()
		leaveShift := int(float64(scaleInt(b.dpi, islandRowHeight)) * b.leaveProgress)
		for visibleIndex, itemIndex := range pageIndices(len(items), b.scroll, islandMaxRows) {
			item := items[itemIndex]
			top := scaleInt(b.dpi, 58) + visibleIndex*scaleInt(b.dpi, islandRowHeight)
			if leavingIndex >= 0 && visibleIndex > leavingIndex {
				top -= leaveShift
			}
			cardLeft := int32(scaleInt(b.dpi, 10))
			cardRight := int32(b.width - scaleInt(b.dpi, 10))
			cardTop := int32(top + scaleInt(b.dpi, 4))
			cardBottom := int32(top + scaleInt(b.dpi, 48))
			if visibleIndex == leavingIndex {
				cardBottom = cardTop + int32(float64(scaleInt(b.dpi, 44))*(1-b.leaveProgress))
			}
			if cardBottom < cardTop {
				cardBottom = cardTop
			}
			cardCorner := int32(scaleInt(b.dpi, 20))
			cardFill := uintptr(0x00202020)
			cardBorder := uintptr(0x00333333)
			if visibleIndex == b.hoverRow && b.leavingID == "" {
				// Only the border changes; the card surface stays put so the
				// small action pill is the single hover target.
				cardBorder = 0x004A4A4A
			}
			// Row card: slightly lighter fill + 1px border.
			if cardBrush, _, _ := islandProcCreateSolidBrush.Call(cardFill); cardBrush != 0 {
				if cardPen, _, _ := islandProcCreatePen.Call(0, 1, cardBorder); cardPen != 0 {
					oldPen, _, _ := islandProcSelectObject.Call(hdc, cardPen)
					oldBrush, _, _ := islandProcSelectObject.Call(hdc, cardBrush)
					islandProcRoundRect.Call(hdc, uintptr(cardLeft), uintptr(cardTop), uintptr(cardRight), uintptr(cardBottom), uintptr(cardCorner), uintptr(cardCorner))
					islandProcSelectObject.Call(hdc, oldBrush)
					islandProcSelectObject.Call(hdc, oldPen)
					islandProcDeleteObject.Call(cardPen)
				}
				islandProcDeleteObject.Call(cardBrush)
			}
			sub := ""
			status := normalizedTrayStatus(item.Status)
			switch status {
			case statusRunning:
				sub = formatRemaining(item.EndsAt, time.Now())
			case statusPaused:
				sub = formatSeconds(item.RemainingSeconds) + " 已暂停"
			case statusPending:
				sub = "已到期"
			}
			if status == statusPending {
				islandProcSetTextColor.Call(hdc, orange)
			}
			// Single line per row: timer on the left, the two actions
			// centred in their click zones on the same line.
			drawIslandText(hdc, item.Title+"  "+sub, rect{left: cardLeft + int32(scaleInt(b.dpi, 4)), top: cardTop, right: int32(b.width - scaleInt(b.dpi, 170)), bottom: cardBottom}, dtLeft|dtVCenter|dtSingleLine|dtEndEllipsis)
			if status == statusPending {
				islandProcSetTextColor.Call(hdc, white)
			}
			left, right := "暂停", "取消"
			switch status {
			case statusPaused:
				left, right = "继续", "取消"
			case statusPending:
				left, right = "已知晓", "稍后提醒"
			}
			firstRect := rect{left: int32(b.scale(islandActionStartX)), top: cardTop, right: int32(b.scale(islandActionSplitX)), bottom: cardBottom}
			secondRect := rect{left: int32(b.scale(islandActionSplitX)), top: cardTop, right: cardRight - int32(scaleInt(b.dpi, 8)), bottom: cardBottom}
			if visibleIndex == b.hoverRow && b.leavingID == "" && (b.hoverHalf == 0 || b.hoverHalf == 1) {
				// The label is centred in its own action zone and the pill
				// is centred on the label (6px padding each side), clamped
				// inside the zone so it never reaches into the other
				// action's label. The previous right-aligned pill put all
				// its padding on the left and overlapped the first action.
				label, zone := left, firstRect
				if b.hoverHalf == 1 {
					label, zone = right, secondRect
				}
				pad := int32(scaleInt(b.dpi, 6))
				textW := int32(b.textExtent(hdc, label))
				textLeft := zone.left + (zone.right-zone.left-textW)/2
				pillHeight := int32(scaleInt(b.dpi, 28))
				pillRect := rect{
					left: textLeft - pad, top: cardTop + (cardBottom-cardTop-pillHeight)/2,
					right: textLeft + textW + pad, bottom: cardTop + (cardBottom-cardTop-pillHeight)/2 + pillHeight,
				}
				if b.hoverHalf == 0 {
					if pillRect.right > firstRect.right-2 {
						pillRect.right = firstRect.right - 2
					}
				} else {
					if pillRect.left < secondRect.left+2 {
						pillRect.left = secondRect.left + 2
					}
				}
				if pillRect.right > cardRight-2 {
					pillRect.right = cardRight - 2
				}
				if pillRect.left < cardLeft+2 {
					pillRect.left = cardLeft + 2
				}
				if pill, _, _ := islandProcCreateSolidBrush.Call(uintptr(0x003A3A3A)); pill != 0 {
					if pen, _, _ := islandProcCreatePen.Call(0, 1, uintptr(0x005A5A5A)); pen != 0 {
						oldPen, _, _ := islandProcSelectObject.Call(hdc, pen)
						oldBrush, _, _ := islandProcSelectObject.Call(hdc, pill)
						islandProcRoundRect.Call(hdc, uintptr(pillRect.left), uintptr(pillRect.top), uintptr(pillRect.right), uintptr(pillRect.bottom), uintptr(scaleInt(b.dpi, 14)), uintptr(scaleInt(b.dpi, 14)))
						islandProcSelectObject.Call(hdc, oldBrush)
						islandProcSelectObject.Call(hdc, oldPen)
						islandProcDeleteObject.Call(pen)
					}
					islandProcDeleteObject.Call(pill)
				}
			}
			drawIslandText(hdc, left, firstRect, dtCenter|dtVCenter|dtSingleLine)
			drawIslandText(hdc, right, secondRect, dtCenter|dtVCenter|dtSingleLine)
		}
		if divBrush, _, _ := islandProcCreateSolidBrush.Call(uintptr(0x002A2A2A)); divBrush != 0 {
			divRect := rect{left: int32(scaleInt(b.dpi, 12)), top: client.bottom - int32(scaleInt(b.dpi, 42)), right: client.right - int32(scaleInt(b.dpi, 12)), bottom: client.bottom - int32(scaleInt(b.dpi, 42)) + 1}
			islandProcFillRect.Call(hdc, uintptr(unsafe.Pointer(&divRect)), divBrush)
			islandProcDeleteObject.Call(divBrush)
		}
		drawIslandText(hdc, "打开列表", rect{left: int32(scaleInt(b.dpi, 12)), top: client.bottom - int32(scaleInt(b.dpi, 38)), right: int32(b.width / 2), bottom: client.bottom}, dtLeft|dtVCenter|dtSingleLine)
		if b.hasPending {
			drawIslandText(hdc, "全部已知晓", rect{left: int32(b.width / 2), top: client.bottom - int32(scaleInt(b.dpi, 38)), right: client.right - int32(scaleInt(b.dpi, 12)), bottom: client.bottom}, dtRight|dtVCenter|dtSingleLine)
		}
	}
	if oldFont != 0 {
		islandProcSelectObject.Call(hdc, oldFont)
	}
}

func scaleInt(dpi, value int) int {
	if dpi <= 0 {
		dpi = 96
	}
	return value * dpi / 96
}

func drawIslandText(hdc uintptr, text string, bounds rect, flags uintptr) {
	ptr, err := syscall.UTF16PtrFromString(text)
	if err != nil {
		return
	}
	islandProcDrawText.Call(hdc, uintptr(unsafe.Pointer(ptr)), uintptr(^uint32(0)), uintptr(unsafe.Pointer(&bounds)), flags)
}
