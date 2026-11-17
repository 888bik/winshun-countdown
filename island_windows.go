//go:build windows

package main

import (
	"math"
	"runtime"
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
	callbacks     islandCallbacks
	ready         chan bool
	cmdMu         sync.Mutex
	commands      []islandCommand
	hwnd          uintptr
	enabled       bool
	visible       atomic.Bool
	expanded      bool
	hasPending    bool
	items         []trayTimerSnapshot
	scroll        int
	dpi           int
	width         int
	height        int
	font          uintptr
	mouseTracking bool
	anim          animation
	animating     bool
	alpha         int
	failed        bool
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
	b := &islandWinBackend{callbacks: callbacks, ready: make(chan bool, 1), dpi: 96, width: islandBaseWidth, height: islandBaseHeight}
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
	if result, _, _ := islandProcSetLayeredWindowAttributes.Call(hwnd, 0, 238, 0x2); result == 0 {
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
		case islandWMMouseHover:
			activeIsland.expanded = true
			activeIsland.layout()
			activeIsland.startIslandAnimation(animEnter)
			return 0
		case islandWMMouseLeave:
			activeIsland.mouseTracking = false
			islandProcSetTimer.Call(hwnd, islandTimerLeave, 300, 0)
			return 0
		case wmTimer:
			switch wParam {
			case islandTimerLeave:
				islandProcKillTimer.Call(hwnd, islandTimerLeave)
				if !activeIsland.pointerInside() {
					activeIsland.expanded = false
					activeIsland.layout()
					activeIsland.startIslandAnimation(animExit)
				}
			case 2:
				// Warm orange breathing pulse while at least one timer awaits acknowledgement.
				phase := float64(time.Now().UnixNano()%int64(1400*time.Millisecond)) / float64(1400*time.Millisecond)
				alpha := byte(190 + int(45*(0.5+0.5*math.Sin(phase*2*math.Pi))))
				islandProcSetLayeredWindowAttributes.Call(hwnd, 0, uintptr(alpha), 0x2)
			case 3:
				progress, done := activeIsland.anim.progress(time.Now())
				alpha := byte(160 + int(78*progress))
				islandProcSetLayeredWindowAttributes.Call(hwnd, 0, uintptr(alpha), 0x2)
				if done {
					islandProcKillTimer.Call(hwnd, 3)
					activeIsland.animating = false
					activeIsland.alpha = 238
					islandProcSetLayeredWindowAttributes.Call(hwnd, 0, 238, 0x2)
				}
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
	duration := animationEnterDuration
	from, to := 0.72, 1.0
	if kind == animExit {
		duration = animationExitDuration
		from, to = 1.0, 0.72
	}
	b.anim.begin(kind, time.Now(), duration, from, to)
	b.animating = true
	islandProcSetTimer.Call(b.hwnd, 3, animationFrameInterval, 0)
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
		b.alpha = 238
		islandProcSetLayeredWindowAttributes.Call(b.hwnd, 0, 238, 0x2)
	}
	b.layout()
}

func (b *islandWinBackend) hideOnGUI() {
	b.visible.Store(false)
	b.expanded = false
	b.scroll = 0
	if b.hwnd != 0 {
		islandProcKillTimer.Call(b.hwnd, 2)
		islandProcKillTimer.Call(b.hwnd, 3)
		islandProcShowWindow.Call(b.hwnd, swHide)
	}
}

func (b *islandWinBackend) layout() {
	if b.hwnd == 0 {
		return
	}
	width, height := islandBaseWidth, islandBaseHeight
	if b.expanded {
		width = islandExpandedWidth
		rows := len(b.visibleItems())
		if rows > islandMaxRows {
			rows = islandMaxRows
		}
		height = 58 + rows*islandRowHeight + 42
	}
	b.width, b.height = b.scale(width), b.scale(height)
	screenW, _, _ := islandProcGetSystemMetrics.Call(0)
	x := (int(screenW) - b.width) / 2
	y := 0
	corner := b.scale(36)
	if b.expanded {
		corner = b.scale(56)
	}
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
	if b.font != 0 {
		islandProcDeleteObject.Call(b.font)
	}
	b.font, _, _ = islandProcCreateFont.Call(uintptr(-b.scale(16)), 0, 0, 0, 600, 0, 0, 0, 1, 0, 0, 5, 0, uintptr(unsafe.Pointer(mustUTF16("Segoe UI"))))
	if result, _, _ := islandProcSetWindowPos.Call(b.hwnd, 0, uintptr(x), uintptr(y), uintptr(b.width), uintptr(b.height), islandSWPNoActivate|islandSWPNoZOrder|islandSWPShowWindow); result == 0 {
		b.failOnGUI()
		return
	}
	islandProcInvalidateRect.Call(b.hwnd, 0, 1)
	islandProcShowWindow.Call(b.hwnd, swShowNoActivate)
	b.visible.Store(true)
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

func (b *islandWinBackend) handleClick(x, y int) {
	if !b.expanded {
		b.expanded = true
		b.layout()
		return
	}
	rowHeight := b.scale(islandRowHeight)
	startY := b.scale(58)
	items := b.visibleItems()
	page := pageIndices(len(items), b.scroll, islandMaxRows)
	for visibleIndex, itemIndex := range page {
		top := startY + visibleIndex*rowHeight
		if y < top || y >= top+rowHeight {
			continue
		}
		item := items[itemIndex]
		first, second := islandRowActions(item.Status)
		if x >= b.scale(170) && x < b.scale(255) {
			b.invoke(item.ID, first)
		} else if x >= b.scale(255) {
			b.invoke(item.ID, second)
		}
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
		if b.hasPending {
			islandProcSetTextColor.Call(hdc, orange)
		}
		model := buildIslandViewModel(b.items, b.hasPending, true, false, time.Now())
		drawIslandText(hdc, model.CollapsedText, rect{left: 0, top: 0, right: client.right, bottom: client.bottom}, dtCenter|dtVCenter|dtSingleLine)
	} else {
		drawIslandText(hdc, "倒计时", rect{left: int32(scaleInt(b.dpi, 16)), top: int32(scaleInt(b.dpi, 8)), right: client.right, bottom: int32(scaleInt(b.dpi, 42))}, dtLeft|dtVCenter|dtSingleLine)
		items := b.visibleItems()
		for visibleIndex, itemIndex := range pageIndices(len(items), b.scroll, islandMaxRows) {
			item := items[itemIndex]
			top := scaleInt(b.dpi, 58) + visibleIndex*scaleInt(b.dpi, islandRowHeight)
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
			drawIslandText(hdc, item.Title+"  "+sub, rect{left: int32(scaleInt(b.dpi, 14)), top: int32(top), right: int32(b.width - scaleInt(b.dpi, 170)), bottom: int32(top + scaleInt(b.dpi, 30))}, dtLeft|dtVCenter|dtSingleLine|dtEndEllipsis)
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
			drawIslandText(hdc, left, rect{left: int32(b.scale(170)), top: int32(top), right: int32(b.scale(247)), bottom: int32(top + scaleInt(b.dpi, 30))}, dtRight|dtVCenter|dtSingleLine)
			drawIslandText(hdc, right, rect{left: int32(b.scale(255)), top: int32(top), right: client.right - int32(scaleInt(b.dpi, 8)), bottom: int32(top + scaleInt(b.dpi, 30))}, dtRight|dtVCenter|dtSingleLine)
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
