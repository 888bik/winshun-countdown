//go:build windows

package main

import (
	"fmt"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

const (
	wmDestroy        = 0x0002
	wmClose          = 0x0010
	wmPaint          = 0x000F
	wmEraseBkgnd     = 0x0014
	wmMouseMove      = 0x0200
	wmLButtonDown    = 0x0201
	wmKeyDown        = 0x0100
	wmVScroll        = 0x0115
	wmMouseWheel     = 0x020A
	wmLButtonUp      = 0x0202
	wmDpiChanged     = 0x02E0
	wmQuit           = 0x0012
	wmCommand        = 0x0111
	wmSetFont        = 0x0030
	wmTimer          = 0x0113
	wmAppCommand     = 0x8000 + 1
	wmPowerBroadcast = 0x0218
	wmTimeChange     = 0x001E
	bsPushButton     = 0x00000000
	wsChild          = 0x40000000
	wsVisible        = 0x10000000
	bnClicked        = 0
	wsCaption        = 0x00C00000
	wsSysMenu        = 0x00080000
	wsVScroll        = 0x00200000
	wsExTool         = 0x00000080
	wsExTopmost      = 0x00000008
	swHide           = 0
	swShow           = 5
	swShowNoActivate = 4
	sbVert           = 1
	sifRangePage     = 0x0003
	sifPos           = 0x0004
	dtLeft           = 0x0000
	dtCenter         = 0x0001
	dtVCenter        = 0x0004
	dtSingleLine     = 0x0020
	dtEndEllipsis    = 0x8000
	dtNoPrefix       = 0x0800
	swpNoZOrder      = 0x0004
	swpNoActivate    = 0x0010
	swpNoSize        = 0x0001
)

type point struct {
	x, y int32
}

type rect struct {
	left, top, right, bottom int32
}

type msg struct {
	hwnd     uintptr
	message  uint32
	wParam   uintptr
	lParam   uintptr
	time     uint32
	pt       point
	lPrivate uint32
}

type wndClassEx struct {
	cbSize        uint32
	style         uint32
	lpfnWndProc   uintptr
	cbClsExtra    int32
	cbWndExtra    int32
	hInstance     uintptr
	hIcon         uintptr
	hCursor       uintptr
	hbrBackground uintptr
	lpszMenuName  *uint16
	lpszClassName *uint16
	hIconSm       uintptr
}

type paintStruct struct {
	hdc         uintptr
	fErase      int32
	rcPaint     rect
	fRestore    int32
	fIncUpdate  int32
	rgbReserved [32]byte
}

type monitorInfo struct {
	cbSize    uint32
	rcMonitor rect
	rcWork    rect
	dwFlags   uint32
}

type scrollInfo struct {
	cbSize    uint32
	fMask     uint32
	nMin      int32
	nMax      int32
	nPage     uint32
	nPos      int32
	nTrackPos int32
}

var (
	user32   = syscall.NewLazyDLL("user32.dll")
	gdi32    = syscall.NewLazyDLL("gdi32.dll")
	kernel32 = syscall.NewLazyDLL("kernel32.dll")

	procRegisterClassExW             = user32.NewProc("RegisterClassExW")
	procCreateWindowExW              = user32.NewProc("CreateWindowExW")
	procDefWindowProcW               = user32.NewProc("DefWindowProcW")
	procDestroyWindow                = user32.NewProc("DestroyWindow")
	procSendMessageW                 = user32.NewProc("SendMessageW")
	procAdjustWindowRectEx           = user32.NewProc("AdjustWindowRectEx")
	procGetWindowLongPtrW            = user32.NewProc("GetWindowLongPtrW")
	procGetCursorPos                 = user32.NewProc("GetCursorPos")
	procGetWindowRect                = user32.NewProc("GetWindowRect")
	procShowWindow                   = user32.NewProc("ShowWindow")
	procUpdateWindow                 = user32.NewProc("UpdateWindow")
	procSetForegroundWindow          = user32.NewProc("SetForegroundWindow")
	procGetMessageW                  = user32.NewProc("GetMessageW")
	procPostMessageW                 = user32.NewProc("PostMessageW")
	procSetTimer                     = user32.NewProc("SetTimer")
	procKillTimer                    = user32.NewProc("KillTimer")
	procSetLayeredWindowAttributes   = user32.NewProc("SetLayeredWindowAttributes")
	procSystemParametersInfoW        = user32.NewProc("SystemParametersInfoW")
	procPeekMessageW                 = user32.NewProc("PeekMessageW")
	procTranslateMessage             = user32.NewProc("TranslateMessage")
	procDispatchMessageW             = user32.NewProc("DispatchMessageW")
	procPostQuitMessage              = user32.NewProc("PostQuitMessage")
	procGetClientRect                = user32.NewProc("GetClientRect")
	procBeginPaint                   = user32.NewProc("BeginPaint")
	procEndPaint                     = user32.NewProc("EndPaint")
	procInvalidateRect               = user32.NewProc("InvalidateRect")
	procFillRect                     = user32.NewProc("FillRect")
	procFrameRect                    = user32.NewProc("FrameRect")
	procSetTextColor                 = gdi32.NewProc("SetTextColor")
	procGetSysColor                  = user32.NewProc("GetSysColor")
	procSetBkMode                    = gdi32.NewProc("SetBkMode")
	procDrawTextW                    = user32.NewProc("DrawTextW")
	procCreateSolidBrush             = gdi32.NewProc("CreateSolidBrush")
	procDeleteObject                 = gdi32.NewProc("DeleteObject")
	procSelectObject                 = gdi32.NewProc("SelectObject")
	procCreateFontW                  = gdi32.NewProc("CreateFontW")
	procSetWindowPos                 = user32.NewProc("SetWindowPos")
	procGetDpiForWindow              = user32.NewProc("GetDpiForWindow")
	procGetDpiForSystem              = user32.NewProc("GetDpiForSystem")
	procSetProcessDpiAwareness       = user32.NewProc("SetProcessDpiAwarenessContext")
	procMonitorFromWindow            = user32.NewProc("MonitorFromWindow")
	procGetMonitorInfoW              = user32.NewProc("GetMonitorInfoW")
	procSetScrollInfo                = user32.NewProc("SetScrollInfo")
	procShowScrollBar                = user32.NewProc("ShowScrollBar")
	procLoadCursorW                  = user32.NewProc("LoadCursorW")
	procLoadIconW                    = user32.NewProc("LoadIconW")
	procGetModuleHandleW             = kernel32.NewProc("GetModuleHandleW")
	procGetSystemMetrics             = user32.NewProc("GetSystemMetrics")
	activeWindowBackend              *winBackend
	windowClassRegistered            bool
	windowClassRegistrationAttempted bool
	windowClassMu                    sync.Mutex
)

type winCommand struct {
	kind       int
	items      []pendingItem
	seconds    int
	animations bool
}

const (
	winShow = iota + 1
	winRefresh
	winHide
	winAutoClose
	winAnimations
)

type winBackend struct {
	callbacks windowCallbacks
	once      sync.Once

	cmdMu       sync.Mutex
	pendingCmds []winCommand

	visible      atomic.Bool
	autoClose    atomic.Int64
	lastActivity time.Time
	animations   bool
	alpha        int
	anim         animation
	animTimer    bool
	baseX        int
	baseY        int

	// The fields below are owned by the GUI thread after create.
	hwnd            uintptr
	items           []pendingItem
	scroll          int
	dpi             int
	fontTitle       uintptr
	fontSmall       uintptr
	fontButton      uintptr
	rowRects        []rect
	allRect         rect
	rowButtons      []uintptr
	snoozeButtons   []uintptr
	allButton       uintptr
	allSnoozeButton uintptr
}

func newWindowBackend(callbacks windowCallbacks) windowBackend {
	return &winBackend{
		callbacks: callbacks,
		dpi:       96,
	}
}

func (b *winBackend) post(command winCommand) {
	b.cmdMu.Lock()
	b.pendingCmds = append(b.pendingCmds, command)
	hwnd := b.hwnd
	b.cmdMu.Unlock()
	if hwnd != 0 {
		procPostMessageW.Call(hwnd, wmAppCommand, 0, 0)
	}
}

func (b *winBackend) drainCommands() {
	b.cmdMu.Lock()
	commands := b.pendingCmds
	b.pendingCmds = nil
	b.cmdMu.Unlock()
	for _, command := range commands {
		switch command.kind {
		case winShow:
			b.showOnGUI(command.items)
		case winRefresh:
			b.refreshOnGUI(command.items)
		case winHide:
			b.hideOnGUI()
		case winAutoClose:
			b.autoClose.Store(int64(command.seconds))
			b.lastActivity = time.Now()
			b.applyAutoCloseTimer()
		case winAnimations:
			b.animations = command.animations
		}
	}
}

func (b *winBackend) Show(items []pendingItem) bool {
	b.once.Do(func() {
		go b.loop()
	})
	b.post(winCommand{kind: winShow, items: append([]pendingItem(nil), items...)})
	return true
}

func (b *winBackend) Refresh(items []pendingItem) {
	b.post(winCommand{kind: winRefresh, items: append([]pendingItem(nil), items...)})
}

func (b *winBackend) Hide() {
	b.post(winCommand{kind: winHide})
}

func (b *winBackend) SetAutoClose(seconds int) {
	b.post(winCommand{kind: winAutoClose, seconds: seconds})
}

func (b *winBackend) SetAnimations(enabled bool) {
	b.post(winCommand{kind: winAnimations, animations: enabled})
}

func (b *winBackend) Visible() bool {
	return b.visible.Load()
}

func (b *winBackend) loop() {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if !b.ensureWindow() {
		if b.callbacks.Failed != nil {
			b.callbacks.Failed()
		}
		return
	}
	b.drainCommands()
	for {
		var message msg
		result, _, _ := procGetMessageW.Call(
			uintptr(unsafe.Pointer(&message)), 0, 0, 0,
		)
		if int32(result) <= 0 {
			return
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&message)))
		procDispatchMessageW.Call(uintptr(unsafe.Pointer(&message)))
	}
}

func ensureDpiAware() {
	// DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2 is -4.
	if procSetProcessDpiAwareness.Find() == nil {
		if result, _, err := procSetProcessDpiAwareness.Call(^uintptr(3)); result == 0 {
			// The host may already have set awareness; that is fine.
			_ = err
		}
	}
}

func initDpiAwareness() {
	ensureDpiAware()
}

func className() *uint16 {
	name, _ := syscall.UTF16PtrFromString("MeTodoCountdownFloatingWindow")
	return name
}

func (b *winBackend) ensureWindow() bool {
	if b.hwnd != 0 {
		return true
	}
	ensureDpiAware()
	windowClassMu.Lock()
	if !windowClassRegistrationAttempted {
		windowClassRegistrationAttempted = true
		instance, _, _ := procGetModuleHandleW.Call(0)
		cursor, _, _ := procLoadCursorW.Call(0, 32512)
		icon, _, _ := procLoadIconW.Call(0, 32512)
		wc := wndClassEx{
			cbSize:        uint32(unsafe.Sizeof(wndClassEx{})),
			lpfnWndProc:   syscall.NewCallback(windowProc),
			hInstance:     instance,
			hIcon:         icon,
			hCursor:       cursor,
			hbrBackground: 6, // COLOR_WINDOW + 1
			lpszClassName: className(),
			hIconSm:       icon,
		}
		result, _, err := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc)))
		if result == 0 {
			if errno, ok := err.(syscall.Errno); !ok || errno != 1410 {
				fmt.Fprintf(os.Stderr, "me.countdown: RegisterClassExW failed: %v\n", err)
			}
		}
		windowClassRegistered = result != 0
	}
	windowClassMu.Unlock()

	activeWindowBackend = b
	style := uintptr(wsCaption | wsSysMenu | wsVScroll)
	exStyle := uintptr(wsExTool | wsExTopmost | 0x00080000) // WS_EX_LAYERED
	instance, _, _ := procGetModuleHandleW.Call(0)
	hwnd, _, err := procCreateWindowExW.Call(
		exStyle, uintptr(unsafe.Pointer(className())),
		uintptr(unsafe.Pointer(mustUTF16("倒计时"))),
		style, 0, 0, 340, 220, 0, 0, instance, 0,
	)
	if hwnd == 0 {
		fmt.Fprintf(os.Stderr, "me.countdown: CreateWindowExW floating window failed: %v\n", err)
		return false
	}
	b.cmdMu.Lock()
	b.hwnd = hwnd
	b.cmdMu.Unlock()
	b.dpi = getDpi(hwnd)
	b.recreateFonts()
	b.alpha = 255
	procSetLayeredWindowAttributes.Call(hwnd, 0, 255, 2) // LWA_ALPHA
	return true
}

func mustUTF16(value string) *uint16 {
	ptr, _ := syscall.UTF16PtrFromString(value)
	return ptr
}

func getDpi(hwnd uintptr) int {
	if procGetDpiForWindow.Find() == nil {
		if dpi, _, _ := procGetDpiForWindow.Call(hwnd); dpi != 0 {
			return int(dpi)
		}
	}
	if procGetDpiForSystem.Find() == nil {
		if dpi, _, _ := procGetDpiForSystem.Call(); dpi != 0 {
			return int(dpi)
		}
	}
	return 96
}

func (b *winBackend) recreateFonts() {
	if b.fontTitle != 0 {
		procDeleteObject.Call(b.fontTitle)
	}
	if b.fontSmall != 0 {
		procDeleteObject.Call(b.fontSmall)
	}
	if b.fontButton != 0 {
		procDeleteObject.Call(b.fontButton)
	}
	scale := float64(b.dpi) / 96.0
	b.fontTitle = createFont(-int(15*scale), 600)
	b.fontSmall = createFont(-int(12*scale), 400)
	b.fontButton = createFont(-int(13*scale), 500)
}

func createFont(height, weight int) uintptr {
	face := mustUTF16("Microsoft YaHei")
	font, _, _ := procCreateFontW.Call(
		uintptr(height), 0, 0, 0, uintptr(weight), 0, 0, 0, 1, 0, 0, 5, 0,
		uintptr(unsafe.Pointer(face)),
	)
	return font
}

func (b *winBackend) showOnGUI(items []pendingItem) {
	if !b.ensureWindow() {
		if b.callbacks.Failed != nil {
			b.callbacks.Failed()
		}
		return
	}
	b.items = append([]pendingItem(nil), items...)
	if !b.visible.Load() {
		b.scroll = 0
	}
	b.layout()
	var windowRect rect
	procGetWindowRect.Call(b.hwnd, uintptr(unsafe.Pointer(&windowRect)))
	b.baseX, b.baseY = int(windowRect.left), int(windowRect.top)
	animate := b.animations && systemAnimationsEnabled()
	if !b.visible.Load() && animate {
		b.alpha = 0
		procSetLayeredWindowAttributes.Call(b.hwnd, 0, 0, 2)
		procSetWindowPos.Call(b.hwnd, 0, uintptr(b.baseX), uintptr(b.baseY+int(16*float64(b.dpi)/96)), 0, 0, swpNoZOrder|swpNoActivate|swpNoSize)
		b.anim.begin(animEnter, time.Now(), 180*time.Millisecond, 0, 1)
		b.ensureAnimationTimer()
	} else if b.anim.kind == animExit {
		b.anim.begin(animEnter, time.Now(), 180*time.Millisecond, float64(b.alpha)/255, 1)
		b.ensureAnimationTimer()
	}
	procShowWindow.Call(b.hwnd, swShowNoActivate)
	procUpdateWindow.Call(b.hwnd)
	b.lastActivity = time.Now()
	b.visible.Store(true)
	b.applyAutoCloseTimer()
	procInvalidateRect.Call(b.hwnd, 0, 0)
}

func systemAnimationsEnabled() bool {
	var enabled int32
	result, _, _ := procSystemParametersInfoW.Call(0x1042, 0, uintptr(unsafe.Pointer(&enabled)), 0)
	return result != 0 && enabled != 0
}

func (b *winBackend) ensureAnimationTimer() {
	if b.animTimer {
		return
	}
	b.animTimer = true
	procSetTimer.Call(b.hwnd, 2, 16, 0)
}

func (b *winBackend) stepAnimation() {
	if b.anim.kind == animNone {
		procKillTimer.Call(b.hwnd, 2)
		b.animTimer = false
		return
	}
	value, done := b.anim.progress(time.Now())
	b.alpha = int(255 * value)
	procSetLayeredWindowAttributes.Call(b.hwnd, 0, uintptr(b.alpha), 2)
	offset := int(16 * float64(b.dpi) / 96)
	y := b.baseY
	if b.anim.kind == animEnter {
		y += int(float64(offset) * (1 - value))
	} else {
		y += int(float64(offset) * value)
	}
	procSetWindowPos.Call(b.hwnd, 0, uintptr(b.baseX), uintptr(y), 0, 0, swpNoZOrder|swpNoActivate|swpNoSize)
	if done {
		procKillTimer.Call(b.hwnd, 2)
		b.animTimer = false
		if b.anim.kind == animExit {
			b.anim.stop()
			procShowWindow.Call(b.hwnd, swHide)
			b.visible.Store(false)
		} else {
			b.anim.stop()
			b.alpha = 255
			procSetLayeredWindowAttributes.Call(b.hwnd, 0, 255, 2)
			procSetWindowPos.Call(b.hwnd, 0, uintptr(b.baseX), uintptr(b.baseY), 0, 0, swpNoZOrder|swpNoActivate|swpNoSize)
		}
	}
}

func (b *winBackend) refreshOnGUI(items []pendingItem) {
	if b.hwnd == 0 || !b.visible.Load() {
		return
	}
	b.items = append([]pendingItem(nil), items...)
	if len(b.items) == 0 {
		b.hideOnGUI()
		return
	}
	b.layout()
	procInvalidateRect.Call(b.hwnd, 0, 0)
}

func (b *winBackend) applyAutoCloseTimer() {
	if b.hwnd == 0 {
		return
	}
	if b.visible.Load() && b.autoClose.Load() > 0 {
		procSetTimer.Call(b.hwnd, 1, 1000, 0)
	} else {
		procKillTimer.Call(b.hwnd, 1)
	}
}

func (b *winBackend) hideOnGUI() {
	if b.visible.Load() && b.animations && systemAnimationsEnabled() && b.anim.kind != animExit {
		procKillTimer.Call(b.hwnd, 1)
		b.anim.begin(animExit, time.Now(), 120*time.Millisecond, float64(b.alpha)/255, 0)
		b.ensureAnimationTimer()
		return
	}
	if b.hwnd != 0 {
		procKillTimer.Call(b.hwnd, 1)
		procKillTimer.Call(b.hwnd, 2)
		b.anim.stop()
		b.animTimer = false
		b.alpha = 255
		procSetLayeredWindowAttributes.Call(b.hwnd, 0, 255, 2)
		procShowWindow.Call(b.hwnd, swHide)
	}
	b.visible.Store(false)
}

func (b *winBackend) layout() {
	if b.hwnd == 0 {
		return
	}
	if dpi := getDpi(b.hwnd); dpi > 0 && dpi != b.dpi {
		b.dpi = dpi
		b.recreateFonts()
	}
	scale := float64(b.dpi) / 96.0
	clientWidth := int(340 * scale)
	rows := len(b.items)
	if rows > 6 {
		rows = 6
	}
	rowHeight := int(52 * scale)
	// Desired CLIENT height: 12 top + rows + 10 gap + 38 all-button + 10 bottom.
	// Let AdjustWindowRectEx add the non-client title bar/frame; using the
	// client rect directly was why the last row and the all-button overlapped.
	clientHeight := int(12*scale) + rows*rowHeight + int(10*scale) + int(38*scale) + int(10*scale)
	var frame rect
	frame.right = int32(clientWidth)
	frame.bottom = int32(clientHeight)
	style, _, _ := procGetWindowLongPtrW.Call(b.hwnd, ^uintptr(15))   // -16
	exStyle, _, _ := procGetWindowLongPtrW.Call(b.hwnd, ^uintptr(19)) // -20
	procAdjustWindowRectEx.Call(uintptr(unsafe.Pointer(&frame)), style, 0, exStyle)
	width := int(frame.right - frame.left)
	height := int(frame.bottom - frame.top)
	if width <= 0 {
		width = clientWidth
	}
	if height <= 0 {
		height = clientHeight
	}
	maxHeight := int(480 * scale)
	if height > maxHeight {
		height = maxHeight
	}
	x, y := b.bottomRight(width, height)
	procSetWindowPos.Call(b.hwnd, ^uintptr(0), uintptr(x), uintptr(y),
		uintptr(width), uintptr(height), swpNoActivate)
	b.updateScroll()
	var client rect
	procGetClientRect.Call(b.hwnd, uintptr(unsafe.Pointer(&client)))
	if client.right <= 0 {
		client.right = int32(clientWidth)
	}
	if client.bottom <= 0 {
		client.bottom = int32(clientHeight)
	}
	b.destroyButtons()
	padding := int(14 * scale)
	buttonWidth := int(84 * scale)
	buttonHeight := int(30 * scale)
	allHeight := int(38 * scale)
	rowTop := int(10 * scale)
	for visibleIndex, index := range pageIndices(len(b.items), b.scroll, 6) {
		top := rowTop + visibleIndex*rowHeight + (rowHeight-buttonHeight)/2
		ackX := int(client.right) - padding - buttonWidth
		snoozeX := ackX - int(8*scale) - buttonWidth
		ackButton := b.createButton("已知晓", 1000+index, ackX, top, buttonWidth, buttonHeight)
		snoozeButton := b.createButton("稍后提醒", 3000+index, snoozeX, top, buttonWidth, buttonHeight)
		b.rowButtons = append(b.rowButtons, ackButton)
		b.snoozeButtons = append(b.snoozeButtons, snoozeButton)
	}
	allTop := int(client.bottom) - int(10*scale) - allHeight
	available := int(client.right) - 2*padding
	gap := int(8 * scale)
	half := (available - gap) / 2
	allSnooze := b.createButton("全部稍后提醒", 4000, padding, allTop, half, allHeight)
	all := b.createButton("全部已知晓", 2000, padding+half+gap, allTop, available-half-gap, allHeight)
	b.allSnoozeButton = allSnooze
	b.allButton = all
}

func (b *winBackend) destroyButtons() {
	for _, button := range b.rowButtons {
		if button != 0 {
			procDestroyWindow.Call(button)
		}
	}
	for _, button := range b.snoozeButtons {
		if button != 0 {
			procDestroyWindow.Call(button)
		}
	}
	b.rowButtons = nil
	b.snoozeButtons = nil
	if b.allButton != 0 {
		procDestroyWindow.Call(b.allButton)
		b.allButton = 0
	}
	if b.allSnoozeButton != 0 {
		procDestroyWindow.Call(b.allSnoozeButton)
		b.allSnoozeButton = 0
	}
}

func (b *winBackend) createButton(text string, id, x, y, width, height int) uintptr {
	instance, _, _ := procGetModuleHandleW.Call(0)
	button, _, _ := procCreateWindowExW.Call(
		0, uintptr(unsafe.Pointer(mustUTF16("BUTTON"))),
		uintptr(unsafe.Pointer(mustUTF16(text))),
		uintptr(wsChild|wsVisible|bsPushButton),
		uintptr(x), uintptr(y), uintptr(width), uintptr(height),
		b.hwnd, uintptr(id), instance, 0,
	)
	if button != 0 && b.fontButton != 0 {
		procSendMessageW.Call(button, wmSetFont, b.fontButton, 1)
	}
	return button
}

func (b *winBackend) bottomRight(width, height int) (int, int) {
	monitor, _, _ := procMonitorFromWindow.Call(b.hwnd, 2) // MONITOR_DEFAULTTONEAREST
	info := monitorInfo{cbSize: uint32(unsafe.Sizeof(monitorInfo{}))}
	if monitor != 0 {
		if ok, _, _ := procGetMonitorInfoW.Call(monitor, uintptr(unsafe.Pointer(&info))); ok != 0 {
			x := int(info.rcWork.right) - width - int(16*float64(b.dpi)/96.0)
			y := int(info.rcWork.bottom) - height - int(16*float64(b.dpi)/96.0)
			if x < int(info.rcWork.left) {
				x = int(info.rcWork.left)
			}
			if y < int(info.rcWork.top) {
				y = int(info.rcWork.top)
			}
			return x, y
		}
	}
	screenW, _, _ := procGetSystemMetrics.Call(0)
	screenH, _, _ := procGetSystemMetrics.Call(1)
	x := int(screenW) - width - int(16*float64(b.dpi)/96.0)
	y := int(screenH) - height - int(48*float64(b.dpi)/96.0)
	if x < 0 {
		x = 0
	}
	if y < 0 {
		y = 0
	}
	return x, y
}

func (b *winBackend) maxScroll() int {
	if len(b.items) <= 6 {
		return 0
	}
	return len(b.items) - 6
}

func (b *winBackend) updateScroll() {
	if b.hwnd == 0 {
		return
	}
	max := b.maxScroll()
	if b.scroll < 0 {
		b.scroll = 0
	}
	if b.scroll > max {
		b.scroll = max
	}
	if len(b.items) <= 6 {
		procShowScrollBar.Call(b.hwnd, sbVert, 0)
		return
	}
	procShowScrollBar.Call(b.hwnd, sbVert, 1)
	info := scrollInfo{
		cbSize: uint32(unsafe.Sizeof(scrollInfo{})),
		fMask:  sifRangePage | sifPos,
		nMin:   0, nMax: int32(len(b.items) - 1), nPage: 6,
		nPos: int32(b.scroll),
	}
	procSetScrollInfo.Call(b.hwnd, sbVert, uintptr(unsafe.Pointer(&info)), 1)
}

func (b *winBackend) scrollBy(delta int) {
	old := b.scroll
	b.scroll += delta
	max := b.maxScroll()
	if b.scroll < 0 {
		b.scroll = 0
	}
	if b.scroll > max {
		b.scroll = max
	}
	if b.scroll != old {
		b.layout()
		procInvalidateRect.Call(b.hwnd, 0, 0)
	}
}

func (b *winBackend) paint() {
	var ps paintStruct
	hdc, _, _ := procBeginPaint.Call(b.hwnd, uintptr(unsafe.Pointer(&ps)))
	if hdc == 0 {
		return
	}
	defer procEndPaint.Call(b.hwnd, uintptr(unsafe.Pointer(&ps)))
	var client rect
	procGetClientRect.Call(b.hwnd, uintptr(unsafe.Pointer(&client)))
	bgColor, _, _ := procGetSysColor.Call(5)    // COLOR_WINDOW
	textColor, _, _ := procGetSysColor.Call(8)  // COLOR_WINDOWTEXT
	grayColor, _, _ := procGetSysColor.Call(17) // COLOR_GRAYTEXT
	background, _, _ := procCreateSolidBrush.Call(bgColor)
	procFillRect.Call(hdc, uintptr(unsafe.Pointer(&client)), background)
	procDeleteObject.Call(background)
	procSetBkMode.Call(hdc, 1)
	scale := float64(b.dpi) / 96.0
	padding := int(14 * scale)
	rowHeight := int(52 * scale)
	buttonWidth := int(84 * scale)
	y := int(10 * scale)
	for visibleIndex := 0; visibleIndex < 6; visibleIndex++ {
		index := b.scroll + visibleIndex
		if index >= len(b.items) {
			break
		}
		item := b.items[index]
		row := rect{left: int32(padding), top: int32(y), right: int32(client.right - int32(padding)), bottom: int32(y + rowHeight - int(4*scale))}
		buttonLeft := row.right - int32(buttonWidth)
		b.drawText(hdc, item.Title, rect{left: row.left, top: row.top + int32(6*scale), right: buttonLeft - int32(8*scale), bottom: row.top + int32(32*scale)}, b.fontTitle, uint32(textColor), dtLeft|dtVCenter|dtSingleLine|dtEndEllipsis|dtNoPrefix)
		b.drawText(hdc, "已结束", rect{left: row.left, top: row.top + int32(28*scale), right: buttonLeft - int32(8*scale), bottom: row.bottom}, b.fontSmall, uint32(grayColor), dtLeft|dtVCenter|dtSingleLine|dtNoPrefix)
		y += rowHeight
	}
}

func (b *winBackend) drawText(hdc uintptr, text string, area rect, font uintptr, color uint32, flags uint32) {
	if font == 0 {
		return
	}
	old, _, _ := procSelectObject.Call(hdc, font)
	procSetTextColor.Call(hdc, uintptr(color))
	ptr := mustUTF16(text)
	procDrawTextW.Call(hdc, uintptr(unsafe.Pointer(ptr)), ^uintptr(0), uintptr(unsafe.Pointer(&area)), uintptr(flags))
	procSelectObject.Call(hdc, old)
}

func windowProc(hwnd, message, wParam, lParam uintptr) uintptr {
	b := activeWindowBackend
	if b == nil {
		result, _, _ := procDefWindowProcW.Call(hwnd, message, wParam, lParam)
		return result
	}
	switch message {
	case wmMouseMove, wmLButtonDown, wmLButtonUp, wmKeyDown, wmMouseWheel, wmVScroll, wmCommand:
		if b.visible.Load() {
			b.lastActivity = time.Now()
		}
	}
	switch message {
	case wmAppCommand:
		b.drainCommands()
		return 0
	case wmTimer:
		if wParam == 2 {
			b.stepAnimation()
			return 0
		}
		if wParam == 1 && autoCloseDue(b.visible.Load(), int(b.autoClose.Load()), time.Since(b.lastActivity)) {
			b.hideOnGUI()
		}
		return 0
	case wmPowerBroadcast, wmTimeChange:
		if b.callbacks.Wake != nil {
			b.callbacks.Wake()
		}
	}
	switch message {
	case wmClose:
		b.hideOnGUI()
		return 0
	case wmDestroy:
		procPostQuitMessage.Call(0)
		return 0
	case wmEraseBkgnd:
		return 1
	case wmPaint:
		b.paint()
		return 0
	case wmCommand:
		if int((wParam>>16)&0xffff) == bnClicked {
			id := int(wParam & 0xffff)
			if id == 2000 {
				if b.callbacks.AckAll != nil {
					b.callbacks.AckAll()
				}
			} else if id == 4000 {
				if b.callbacks.SnoozeAll != nil {
					b.callbacks.SnoozeAll()
				}
			} else if id >= 1000 && id-1000 < len(b.items) {
				if b.callbacks.Ack != nil {
					b.callbacks.Ack(b.items[id-1000].ID)
				}
			} else if id >= 3000 && id-3000 < len(b.items) {
				if b.callbacks.Snooze != nil {
					b.callbacks.Snooze(b.items[id-3000].ID)
				}
			}
		}
		return 0
	case wmVScroll:
		code := int(wParam & 0xffff)
		switch code {
		case 0:
			b.scrollBy(-1)
		case 1:
			b.scrollBy(1)
		case 2:
			b.scrollBy(-6)
		case 3:
			b.scrollBy(6)
		case 5:
			b.scrollBy(-len(b.items))
		case 6:
			b.scrollBy(len(b.items))
		}
		return 0
	case wmMouseWheel:
		delta := int(int16((wParam >> 16) & 0xffff))
		if delta != 0 {
			b.scrollBy(-delta / 120)
		}
		return 0
	case wmDpiChanged:
		newDPI := int(wParam & 0xffff)
		if newDPI > 0 {
			b.dpi = newDPI
		}
		b.recreateFonts()
		b.layout()
		procInvalidateRect.Call(hwnd, 0, 0)
		return 0
	}
	result, _, _ := procDefWindowProcW.Call(hwnd, message, wParam, lParam)
	return result
}
