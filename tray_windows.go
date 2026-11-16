//go:build windows

package main

import (
	"fmt"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"unsafe"
)

const (
	trayNIMAdd    = 0x00000000
	trayNIMModify = 0x00000001
	trayNIMDelete = 0x00000002

	trayNIFMessage = 0x00000001
	trayNIFIcon    = 0x00000002
	trayNIFTip     = 0x00000004

	trayUID = 1

	// window_windows.go uses WM_APP+1 for its own command queue and WM_TIMER
	// for its animation/auto-close work. Keep the tray messages separate.
	trayWMCallback = 0x8000 + 0x40
	trayWMCommand  = 0x8000 + 0x41

	trayWMDestroy      = 0x0002
	trayWMNull         = 0x0000
	trayWMRButtonUp    = 0x0205
	trayWMLButtonUp    = 0x0202
	trayWMContextMenu  = 0x007B
	trayNINSelect      = 0x0400
	trayNINKeySelect   = 0x0401
	trayWSExToolWindow = 0x00000080
	trayWSPopup        = 0x80000000
	trayMFString       = 0x00000000
	trayMFGray         = 0x00000001
	trayMFSeparator    = 0x00000800
	trayMFPopup        = 0x00000010
	trayTPMRightButton = 0x0002
	trayTPMReturnCmd   = 0x0100
	traySMCXSmIcon     = 49
	traySMCYSmIcon     = 50
	trayIDIApplication = 32512
	trayDIBRGBColors   = 0
	trayBI_RGB         = 0
)

type trayMessage struct {
	hwnd     uintptr
	message  uint32
	wParam   uintptr
	lParam   uintptr
	time     uint32
	pt       trayPoint
	lPrivate uint32
}

type trayPoint struct {
	x int32
	y int32
}

type trayWndClassEx struct {
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

type trayNotifyIconData struct {
	cbSize            uint32
	hWnd              uintptr
	uID               uint32
	uFlags            uint32
	uCallbackMessage  uint32
	hIcon             uintptr
	szTip             [128]uint16
	dwState           uint32
	dwStateMask       uint32
	szInfo            [256]uint16
	uTimeoutOrVersion uint32
	szInfoTitle       [64]uint16
	dwInfoFlags       uint32
	guidItem          [16]byte
	hBalloonIcon      uintptr
}

type trayIconInfo struct {
	fIcon    int32
	xHotspot uint32
	yHotspot uint32
	hbmMask  uintptr
	hbmColor uintptr
}

type trayBitmapInfoHeader struct {
	size          uint32
	width         int32
	height        int32
	planes        uint16
	bitCount      uint16
	compression   uint32
	sizeImage     uint32
	xPelsPerMeter int32
	yPelsPerMeter int32
	clrUsed       uint32
	clrImportant  uint32
}

type trayBitmapInfo struct {
	header trayBitmapInfoHeader
	colors [1]uint32
}

type winTrayCommand struct {
	kind    int
	enabled bool
	items   []trayTimerSnapshot
}

const (
	trayCommandSetEnabled = iota + 1
	trayCommandUpdate
	trayCommandTaskbarCreated
	trayCommandClose
)

var (
	trayUser32   = syscall.NewLazyDLL("user32.dll")
	trayShell32  = syscall.NewLazyDLL("shell32.dll")
	trayGDI32    = syscall.NewLazyDLL("gdi32.dll")
	trayKernel32 = syscall.NewLazyDLL("kernel32.dll")

	trayProcRegisterClassExW    = trayUser32.NewProc("RegisterClassExW")
	trayProcCreateWindowExW     = trayUser32.NewProc("CreateWindowExW")
	trayProcDefWindowProcW      = trayUser32.NewProc("DefWindowProcW")
	trayProcGetMessageW         = trayUser32.NewProc("GetMessageW")
	trayProcTranslateMessage    = trayUser32.NewProc("TranslateMessage")
	trayProcDispatchMessageW    = trayUser32.NewProc("DispatchMessageW")
	trayProcPostMessageW        = trayUser32.NewProc("PostMessageW")
	trayProcSendMessageW        = trayUser32.NewProc("SendMessageW")
	trayProcDestroyWindow       = trayUser32.NewProc("DestroyWindow")
	trayProcPostQuitMessage     = trayUser32.NewProc("PostQuitMessage")
	trayProcRegisterWindowMsgW  = trayUser32.NewProc("RegisterWindowMessageW")
	trayProcSetForegroundWindow = trayUser32.NewProc("SetForegroundWindow")
	trayProcGetCursorPos        = trayUser32.NewProc("GetCursorPos")
	trayProcTrackPopupMenu      = trayUser32.NewProc("TrackPopupMenu")
	trayProcCreatePopupMenu     = trayUser32.NewProc("CreatePopupMenu")
	trayProcAppendMenuW         = trayUser32.NewProc("AppendMenuW")
	trayProcDestroyMenu         = trayUser32.NewProc("DestroyMenu")
	trayProcLoadIconW           = trayUser32.NewProc("LoadIconW")
	trayProcCreateIconIndirect  = trayUser32.NewProc("CreateIconIndirect")
	trayProcDestroyIcon         = trayUser32.NewProc("DestroyIcon")
	trayProcGetSystemMetrics    = trayUser32.NewProc("GetSystemMetrics")

	trayProcShellNotifyIconW = trayShell32.NewProc("Shell_NotifyIconW")
	trayProcCreateDIBSection = trayGDI32.NewProc("CreateDIBSection")
	trayProcCreateBitmap     = trayGDI32.NewProc("CreateBitmap")
	trayProcDeleteObject     = trayGDI32.NewProc("DeleteObject")
	trayProcGetModuleHandleW = trayKernel32.NewProc("GetModuleHandleW")

	activeTrayBackend atomic.Pointer[winTrayBackend]
)

type winTrayBackend struct {
	callbacks trayCallbacks

	startOnce     sync.Once
	closeOnce     sync.Once
	ready         chan struct{}
	done          chan struct{}
	started       atomic.Bool
	closed        atomic.Bool
	hwnd          atomic.Uintptr
	broadcastHwnd atomic.Uintptr

	cmdMu       sync.Mutex
	pendingCmds []winTrayCommand

	// The following fields are owned by the tray GUI thread.
	enabled              bool
	iconAdded            bool
	icon                 uintptr
	ownedIcon            bool
	iconMinuteValue      string
	iconValueInitialized bool
	taskbarMsg           uint32
	items                []trayTimerSnapshot
}

func newTrayBackend(callbacks trayCallbacks) trayBackend {
	return &winTrayBackend{
		callbacks: callbacks,
		ready:     make(chan struct{}),
		done:      make(chan struct{}),
	}
}

func (b *winTrayBackend) ensureThread() bool {
	if b.closed.Load() {
		return false
	}
	b.startOnce.Do(func() {
		if b.closed.Load() {
			return
		}
		b.started.Store(true)
		go b.loop()
	})
	if !b.started.Load() {
		return false
	}
	<-b.ready
	return b.hwnd.Load() != 0 && !b.closed.Load()
}

func (b *winTrayBackend) post(command winTrayCommand) {
	b.cmdMu.Lock()
	if b.closed.Load() {
		b.cmdMu.Unlock()
		return
	}
	b.pendingCmds = append(b.pendingCmds, command)
	hwnd := b.hwnd.Load()
	b.cmdMu.Unlock()
	wakeTrayThread(hwnd)
}

func wakeTrayThread(hwnd uintptr) {
	if hwnd == 0 {
		return
	}
	posted, _, _ := trayProcPostMessageW.Call(hwnd, trayWMCommand, 0, 0)
	if posted == 0 {
		// PostMessage can fail when the thread queue is full. A synchronous
		// fallback guarantees that shutdown and the latest state update are not
		// stranded in the command queue; WndProc still runs on the GUI thread.
		trayProcSendMessageW.Call(hwnd, trayWMCommand, 0, 0)
	}
}

func (b *winTrayBackend) SetEnabled(enabled bool) {
	if enabled {
		if !b.ensureThread() {
			return
		}
	} else if !b.started.Load() || b.closed.Load() {
		return
	}
	b.post(winTrayCommand{kind: trayCommandSetEnabled, enabled: enabled})
}

func (b *winTrayBackend) Update(items []trayTimerSnapshot) {
	if !b.started.Load() || b.closed.Load() {
		return
	}
	b.post(winTrayCommand{kind: trayCommandUpdate, items: append([]trayTimerSnapshot(nil), items...)})
}

// TaskbarCreated is also exposed through the backend interface so the
// non-Windows fake can exercise Explorer-restart recovery deterministically.
func (b *winTrayBackend) TaskbarCreated() {
	if !b.started.Load() || b.closed.Load() {
		return
	}
	b.post(winTrayCommand{kind: trayCommandTaskbarCreated})
}

func (b *winTrayBackend) Close() {
	b.closeOnce.Do(func() {
		b.cmdMu.Lock()
		b.closed.Store(true)
		if !b.started.Load() {
			b.cmdMu.Unlock()
			return
		}
		b.pendingCmds = append(b.pendingCmds, winTrayCommand{kind: trayCommandClose})
		hwnd := b.hwnd.Load()
		b.cmdMu.Unlock()
		wakeTrayThread(hwnd)
		<-b.done
	})
}

func (b *winTrayBackend) loop() {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	defer close(b.done)

	activeTrayBackend.Store(b)
	defer activeTrayBackend.CompareAndSwap(b, nil)
	instance, _, _ := trayProcGetModuleHandleW.Call(0)
	className, _ := syscall.UTF16PtrFromString("MeCountdownTrayMessageWindow")
	class := trayWndClassEx{
		cbSize:        uint32(unsafe.Sizeof(trayWndClassEx{})),
		lpfnWndProc:   syscall.NewCallback(trayWindowProc),
		hInstance:     instance,
		lpszClassName: className,
	}
	registered, _, registerErr := trayProcRegisterClassExW.Call(uintptr(unsafe.Pointer(&class)))
	if registered == 0 {
		if errno, ok := registerErr.(syscall.Errno); !ok || errno != 1410 {
			fmt.Fprintf(os.Stderr, "me.countdown: tray RegisterClassExW failed: %v\n", registerErr)
		}
	}

	title, _ := syscall.UTF16PtrFromString("me.countdown tray")
	const hwndMessage = ^uintptr(2) // HWND_MESSAGE = (HWND)-3.
	hwnd, _, createErr := trayProcCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(className)),
		uintptr(unsafe.Pointer(title)),
		0,
		0, 0, 0, 0,
		hwndMessage,
		0,
		instance,
		0,
	)
	if hwnd == 0 {
		fmt.Fprintf(os.Stderr, "me.countdown: tray CreateWindowExW failed: %v\n", createErr)
		close(b.ready)
		return
	}
	b.hwnd.Store(hwnd)
	messageName, _ := syscall.UTF16PtrFromString("TaskbarCreated")
	registeredMessage, _, _ := trayProcRegisterWindowMsgW.Call(uintptr(unsafe.Pointer(messageName)))
	b.taskbarMsg = uint32(registeredMessage)

	// HWND_MESSAGE windows do not receive broadcast messages. Keep a second,
	// invisible WS_EX_TOOLWINDOW top-level window for TaskbarCreated broadcasts
	// and as the foreground/menu owner. The icon callback target remains the
	// HWND_MESSAGE window, and the tool window never enters taskbar/Alt+Tab.
	broadcastTitle, _ := syscall.UTF16PtrFromString("me.countdown taskbar restart listener")
	broadcastHwnd, _, broadcastErr := trayProcCreateWindowExW.Call(
		trayWSExToolWindow,
		uintptr(unsafe.Pointer(className)),
		uintptr(unsafe.Pointer(broadcastTitle)),
		trayWSPopup,
		^uintptr(31999), ^uintptr(31999), 0, 0,
		0, 0, instance, 0,
	)
	if broadcastHwnd == 0 {
		fmt.Fprintf(os.Stderr, "me.countdown: tray TaskbarCreated listener unavailable: %v\n", broadcastErr)
	} else {
		b.broadcastHwnd.Store(broadcastHwnd)
	}
	close(b.ready)

	b.drainCommands()
	for {
		var message trayMessage
		result, _, _ := trayProcGetMessageW.Call(uintptr(unsafe.Pointer(&message)), 0, 0, 0)
		if int32(result) <= 0 {
			break
		}
		trayProcTranslateMessage.Call(uintptr(unsafe.Pointer(&message)))
		trayProcDispatchMessageW.Call(uintptr(unsafe.Pointer(&message)))
	}
	// Delete the shell icon while its callback HWND is still valid. If the
	// message loop exits unexpectedly, also destroy either remaining window.
	if b.iconAdded {
		b.deleteIconOnGUI()
	}
	if hwnd := b.broadcastHwnd.Load(); hwnd != 0 {
		trayProcDestroyWindow.Call(hwnd)
	}
	if hwnd := b.hwnd.Load(); hwnd != 0 {
		trayProcDestroyWindow.Call(hwnd)
	}
	b.broadcastHwnd.Store(0)
	b.hwnd.Store(0)
	if b.icon != 0 && b.ownedIcon {
		trayProcDestroyIcon.Call(b.icon)
	}
	b.icon = 0
	b.ownedIcon = false
	b.iconMinuteValue = ""
	b.iconValueInitialized = false
}

func (b *winTrayBackend) drainCommands() {
	b.cmdMu.Lock()
	commands := b.pendingCmds
	b.pendingCmds = nil
	b.cmdMu.Unlock()
	for _, command := range commands {
		switch command.kind {
		case trayCommandSetEnabled:
			b.enabled = command.enabled
			if b.enabled {
				b.addOrModifyIconOnGUI()
			} else {
				b.deleteIconOnGUI()
			}
		case trayCommandUpdate:
			b.items = append([]trayTimerSnapshot(nil), command.items...)
			if b.enabled {
				b.addOrModifyIconOnGUI()
			}
		case trayCommandTaskbarCreated:
			b.handleTaskbarCreatedOnGUI()
		case trayCommandClose:
			b.enabled = false
			b.deleteIconOnGUI()
			if hwnd := b.broadcastHwnd.Load(); hwnd != 0 {
				trayProcDestroyWindow.Call(hwnd)
			}
			if hwnd := b.hwnd.Load(); hwnd != 0 {
				trayProcDestroyWindow.Call(hwnd)
			}
		}
	}
}

func trayWindowProc(hwnd uintptr, message uint32, wParam, lParam uintptr) uintptr {
	b := activeTrayBackend.Load()
	if b == nil {
		result, _, _ := trayProcDefWindowProcW.Call(hwnd, uintptr(message), wParam, lParam)
		return result
	}
	if b.taskbarMsg != 0 && message == b.taskbarMsg {
		b.handleTaskbarCreatedOnGUI()
		return 0
	}
	switch message {
	case trayWMCommand:
		b.drainCommands()
		return 0
	case trayWMCallback:
		event := uint32(lParam)
		switch event {
		case trayWMRButtonUp, trayWMLButtonUp, trayWMContextMenu, trayNINSelect, trayNINKeySelect:
			b.showMenuOnGUI()
		}
		return 0
	case trayWMDestroy:
		if hwnd == b.hwnd.Load() {
			trayProcPostQuitMessage.Call(0)
		}
		return 0
	}
	result, _, _ := trayProcDefWindowProcW.Call(hwnd, uintptr(message), wParam, lParam)
	return result
}

func (b *winTrayBackend) handleTaskbarCreatedOnGUI() {
	if !b.enabled {
		return
	}
	// Explorer has discarded the notification area state. A fresh NIM_ADD is
	// required; addOrModifyIconOnGUI also falls back to NIM_MODIFY if needed.
	b.iconAdded = false
	b.addOrModifyIconOnGUI()
}

func (b *winTrayBackend) addOrModifyIconOnGUI() {
	if !b.enabled {
		return
	}
	desiredMinuteValue := trayIconMinuteValue(b.items)
	rebuildIcon := b.icon == 0 || trayIconValueChanged(b.iconMinuteValue, b.iconValueInitialized, desiredMinuteValue)
	nextIcon := b.icon
	nextOwned := b.ownedIcon
	if rebuildIcon {
		nextIcon, nextOwned = createCountdownTrayIcon(desiredMinuteValue)
		if nextIcon == 0 {
			fmt.Fprintln(os.Stderr, "me.countdown: unable to create tray icon")
			return
		}
	}
	data := b.notifyData()
	data.hIcon = nextIcon
	operation := uintptr(trayNIMModify)
	if !b.iconAdded {
		operation = trayNIMAdd
	}
	result, _, callErr := trayProcShellNotifyIconW.Call(operation, uintptr(unsafe.Pointer(&data)))
	runtime.KeepAlive(&data)
	if result == 0 && operation == trayNIMAdd {
		// A TaskbarCreated broadcast can also be sent for primary-display DPI
		// changes. If the shell retained the icon, MODIFY is the safe fallback.
		result, _, callErr = trayProcShellNotifyIconW.Call(trayNIMModify, uintptr(unsafe.Pointer(&data)))
		runtime.KeepAlive(&data)
	}
	if result == 0 {
		if rebuildIcon && nextOwned {
			trayProcDestroyIcon.Call(nextIcon)
		}
		fmt.Fprintf(os.Stderr, "me.countdown: Shell_NotifyIconW add/modify failed: %v\n", callErr)
		return
	}
	if rebuildIcon {
		if shouldDestroyOwnedTrayIcon(b.icon, b.ownedIcon, nextIcon) {
			trayProcDestroyIcon.Call(b.icon)
		}
		b.icon = nextIcon
		b.ownedIcon = nextOwned
		b.iconMinuteValue = desiredMinuteValue
		b.iconValueInitialized = true
	}
	b.iconAdded = true
}

func (b *winTrayBackend) deleteIconOnGUI() {
	if !b.iconAdded {
		return
	}
	data := b.notifyData()
	result, _, callErr := trayProcShellNotifyIconW.Call(trayNIMDelete, uintptr(unsafe.Pointer(&data)))
	runtime.KeepAlive(&data)
	if result == 0 {
		_ = callErr
	}
	b.iconAdded = false
}

func (b *winTrayBackend) notifyData() trayNotifyIconData {
	data := trayNotifyIconData{
		cbSize:           uint32(unsafe.Sizeof(trayNotifyIconData{})),
		hWnd:             b.hwnd.Load(),
		uID:              trayUID,
		uFlags:           trayNIFMessage | trayNIFIcon | trayNIFTip,
		uCallbackMessage: trayWMCallback,
		hIcon:            b.icon,
	}
	tooltip, _ := syscall.UTF16FromString(buildTrayTooltip(b.items))
	if len(tooltip) > len(data.szTip) {
		tooltip = tooltip[:len(data.szTip)-1]
	}
	copy(data.szTip[:], tooltip)
	return data
}

func (b *winTrayBackend) showMenuOnGUI() {
	if !b.enabled || b.callbacks.Snapshot == nil {
		return
	}
	// Snapshot obtains and releases Plugin.mu before menu construction. Never
	// hold the business-state lock while TrackPopupMenu runs its modal loop.
	items := b.callbacks.Snapshot()
	model := buildTrayMenuModel(items)
	menu, _, _ := trayProcCreatePopupMenu.Call()
	if menu == 0 {
		return
	}
	defer trayProcDestroyMenu.Call(menu)
	for _, item := range model.Items {
		if !b.appendMenuItem(menu, item) {
			return
		}
	}
	var cursor trayPoint
	trayProcGetCursorPos.Call(uintptr(unsafe.Pointer(&cursor)))
	hwnd := b.hwnd.Load()
	menuOwner := b.broadcastHwnd.Load()
	if menuOwner == 0 {
		menuOwner = hwnd
	}
	trayProcSetForegroundWindow.Call(menuOwner)
	selected, _, _ := trayProcTrackPopupMenu.Call(
		menu,
		trayTPMReturnCmd|trayTPMRightButton,
		uintptr(cursor.x),
		uintptr(cursor.y),
		0,
		menuOwner,
		0,
	)
	trayProcPostMessageW.Call(menuOwner, trayWMNull, 0, 0)
	if selected == 0 {
		return
	}
	binding, ok := model.Commands[uint32(selected)]
	if !ok {
		return
	}
	if binding.OpenList {
		if b.callbacks.OpenList != nil {
			b.callbacks.OpenList()
		}
		return
	}
	if b.callbacks.Action != nil {
		b.callbacks.Action(binding.TimerID, binding.Action)
	}
}

func (b *winTrayBackend) appendMenuItem(menu uintptr, item trayMenuItem) bool {
	if item.Separator {
		result, _, _ := trayProcAppendMenuW.Call(menu, trayMFSeparator, 0, 0)
		return result != 0
	}
	text, err := syscall.UTF16PtrFromString(item.Label)
	if err != nil {
		return false
	}
	flags := uintptr(trayMFString)
	if item.Disabled {
		flags |= trayMFGray
	}
	if len(item.Children) > 0 {
		submenu, _, _ := trayProcCreatePopupMenu.Call()
		if submenu == 0 {
			return false
		}
		for _, child := range item.Children {
			if !b.appendMenuItem(submenu, child) {
				trayProcDestroyMenu.Call(submenu)
				return false
			}
		}
		result, _, _ := trayProcAppendMenuW.Call(menu, flags|trayMFPopup, submenu, uintptr(unsafe.Pointer(text)))
		if result == 0 {
			trayProcDestroyMenu.Call(submenu)
			return false
		}
		return true
	}
	result, _, _ := trayProcAppendMenuW.Call(menu, flags, uintptr(item.CommandID), uintptr(unsafe.Pointer(text)))
	return result != 0
}

func createCountdownTrayIcon(minuteValue string) (uintptr, bool) {
	width, _, _ := trayProcGetSystemMetrics.Call(traySMCXSmIcon)
	height, _, _ := trayProcGetSystemMetrics.Call(traySMCYSmIcon)
	if width == 0 {
		width = 16
	}
	if height == 0 {
		height = 16
	}
	icon, err := createClockIconWithLabel(int(width), int(height), minuteValue)
	if err == nil && icon != 0 {
		return icon, true
	}
	fallback, _, _ := trayProcLoadIconW.Call(0, trayIDIApplication)
	return fallback, false
}

func createClockIcon(width, height int) (uintptr, error) {
	return createClockIconWithLabel(width, height, "")
}

func createClockIconWithLabel(width, height int, minuteValue string) (uintptr, error) {
	if width < 8 || height < 8 || width > 128 || height > 128 {
		return 0, fmt.Errorf("unsupported tray icon size %dx%d", width, height)
	}
	info := trayBitmapInfo{
		header: trayBitmapInfoHeader{
			size:        uint32(unsafe.Sizeof(trayBitmapInfoHeader{})),
			width:       int32(width),
			height:      -int32(height),
			planes:      1,
			bitCount:    32,
			compression: trayBI_RGB,
		},
	}
	var bits unsafe.Pointer
	colorBitmap, _, colorErr := trayProcCreateDIBSection.Call(
		0,
		uintptr(unsafe.Pointer(&info)),
		trayDIBRGBColors,
		uintptr(unsafe.Pointer(&bits)),
		0,
		0,
	)
	if colorBitmap == 0 || bits == nil {
		return 0, fmt.Errorf("CreateDIBSection failed: %v", colorErr)
	}
	defer trayProcDeleteObject.Call(colorBitmap)

	maskStride := ((width + 15) / 16) * 2
	maskBits := make([]byte, maskStride*height)
	maskBitmap, _, maskErr := trayProcCreateBitmap.Call(
		uintptr(width), uintptr(height), 1, 1, uintptr(unsafe.Pointer(&maskBits[0])),
	)
	if maskBitmap == 0 {
		return 0, fmt.Errorf("CreateBitmap mask failed: %v", maskErr)
	}
	defer trayProcDeleteObject.Call(maskBitmap)

	pixels := unsafe.Slice((*byte)(bits), width*height*4)
	clear(pixels) // CreateDIBSection memory is not guaranteed to be zeroed.
	centerX, centerY := width/2, height/2
	radius := minInt(width, height)/2 - 1
	blue := [4]byte{0xFF, 0x7F, 0x2B, 0xFF} // BGRA for #2B7FFF.
	white := [4]byte{0xFF, 0xFF, 0xFF, 0xFF}
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			dx, dy := x-centerX, y-centerY
			if dx*dx+dy*dy <= radius*radius {
				setTrayPixel(pixels, width, x, y, blue)
			}
		}
	}
	if minuteValue == "" {
		drawTrayLine(pixels, width, centerX, centerY, centerX, centerY-radius*3/4, white)
		drawTrayLine(pixels, width, centerX, centerY, centerX+radius*3/5, centerY+radius/3, white)
		setTrayPixel(pixels, width, centerX, centerY, white)
	} else {
		drawTrayMinuteLabel(pixels, width, height, minuteValue, white)
	}

	iconInfo := trayIconInfo{
		fIcon:    1,
		xHotspot: uint32(width / 2),
		yHotspot: uint32(height / 2),
		hbmMask:  maskBitmap,
		hbmColor: colorBitmap,
	}
	icon, _, iconErr := trayProcCreateIconIndirect.Call(uintptr(unsafe.Pointer(&iconInfo)))
	if icon == 0 {
		return 0, fmt.Errorf("CreateIconIndirect failed: %v", iconErr)
	}
	return icon, nil
}

func drawTrayMinuteLabel(pixels []byte, width, height int, label string, color [4]byte) {
	glyphs := map[rune][5]uint8{
		'0': {0b111, 0b101, 0b101, 0b101, 0b111},
		'1': {0b010, 0b110, 0b010, 0b010, 0b111},
		'2': {0b111, 0b001, 0b111, 0b100, 0b111},
		'3': {0b111, 0b001, 0b111, 0b001, 0b111},
		'4': {0b101, 0b101, 0b111, 0b001, 0b001},
		'5': {0b111, 0b100, 0b111, 0b001, 0b111},
		'6': {0b111, 0b100, 0b111, 0b101, 0b111},
		'7': {0b111, 0b001, 0b010, 0b010, 0b010},
		'8': {0b111, 0b101, 0b111, 0b101, 0b111},
		'9': {0b111, 0b101, 0b111, 0b001, 0b111},
		'+': {0b000, 0b010, 0b111, 0b010, 0b000},
	}
	characters := []rune(label)
	if len(characters) == 0 {
		return
	}
	scale := minInt(width, height) / 16
	if scale < 1 {
		scale = 1
	}
	spacing := scale
	glyphWidth := 3 * scale
	glyphHeight := 5 * scale
	totalWidth := len(characters)*glyphWidth + (len(characters)-1)*spacing
	if totalWidth > width-2 {
		return
	}
	startX := (width - totalWidth) / 2
	startY := (height - glyphHeight) / 2
	for index, character := range characters {
		pattern, ok := glyphs[character]
		if !ok {
			continue
		}
		baseX := startX + index*(glyphWidth+spacing)
		for row, bits := range pattern {
			for column := 0; column < 3; column++ {
				if bits&(1<<uint(2-column)) == 0 {
					continue
				}
				for dy := 0; dy < scale; dy++ {
					for dx := 0; dx < scale; dx++ {
						setTrayPixel(pixels, width, baseX+column*scale+dx, startY+row*scale+dy, color)
					}
				}
			}
		}
	}
}

func minInt(left, right int) int {
	if left < right {
		return left
	}
	return right
}

func setTrayPixel(pixels []byte, width, x, y int, color [4]byte) {
	if x < 0 || y < 0 {
		return
	}
	index := (y*width + x) * 4
	if index+3 >= len(pixels) {
		return
	}
	copy(pixels[index:index+4], color[:])
}

func drawTrayLine(pixels []byte, width, x0, y0, x1, y1 int, color [4]byte) {
	dx := absInt(x1 - x0)
	sx := -1
	if x0 < x1 {
		sx = 1
	}
	dy := -absInt(y1 - y0)
	sy := -1
	if y0 < y1 {
		sy = 1
	}
	err := dx + dy
	for {
		setTrayPixel(pixels, width, x0, y0, color)
		if x0 == x1 && y0 == y1 {
			break
		}
		e2 := 2 * err
		if e2 >= dy {
			err += dy
			x0 += sx
		}
		if e2 <= dx {
			err += dx
			y0 += sy
		}
	}
}

func absInt(value int) int {
	if value < 0 {
		return -value
	}
	return value
}
