//go:build windows

package main

import (
	_ "embed"
	"log"
	"runtime"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

//go:embed web/icon-64.png
var trayPNG []byte

var (
	procRegisterClassExW         = user32.NewProc("RegisterClassExW")
	procCreateWindowExW          = user32.NewProc("CreateWindowExW")
	procDefWindowProcW           = user32.NewProc("DefWindowProcW")
	procGetMessageW              = user32.NewProc("GetMessageW")
	procTranslateMessage         = user32.NewProc("TranslateMessage")
	procDispatchMessageW         = user32.NewProc("DispatchMessageW")
	procPostQuitMessage          = user32.NewProc("PostQuitMessage")
	procPostMessageW             = user32.NewProc("PostMessageW")
	procDestroyWindow            = user32.NewProc("DestroyWindow")
	procCreatePopupMenu          = user32.NewProc("CreatePopupMenu")
	procAppendMenuW              = user32.NewProc("AppendMenuW")
	procSetMenuDefaultItem       = user32.NewProc("SetMenuDefaultItem")
	procTrackPopupMenu           = user32.NewProc("TrackPopupMenu")
	procDestroyMenu              = user32.NewProc("DestroyMenu")
	procGetCursorPos             = user32.NewProc("GetCursorPos")
	procLoadIconW                = user32.NewProc("LoadIconW")
	procLoadImageW               = user32.NewProc("LoadImageW")
	procGetSystemMetrics         = user32.NewProc("GetSystemMetrics")
	procCreateIconFromResourceEx = user32.NewProc("CreateIconFromResourceEx")
	procRegisterWindowMessageW   = user32.NewProc("RegisterWindowMessageW")
	procShellNotifyIconW         = shell32.NewProc("Shell_NotifyIconW")
	procGetModuleHandleW         = kernel32.NewProc("GetModuleHandleW")
)

const (
	wmNull          = 0x0000
	wmDestroy       = 0x0002
	wmClose         = 0x0010
	wmLButtonUp     = 0x0202
	wmLButtonDblClk = 0x0203
	wmRButtonUp     = 0x0205
	wmTray          = 0x8000 + 1
	wmTrayUpdate    = 0x8000 + 2

	nimAdd    = 0
	nimModify = 1
	nimDelete = 2

	nifMessage = 0x01
	nifIcon    = 0x02
	nifTip     = 0x04
	nifInfo    = 0x10
	niifInfo   = 0x01

	mfString    = 0x0000
	mfSeparator = 0x0800

	tpmRightButton = 0x0002
	tpmBottomAlign = 0x0020
	tpmReturnCmd   = 0x0100

	menuOpen   = 1
	menuResume = 2
	menuPause  = 3
	menuQuit   = 4
)

type wndClassExW struct {
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

type winPoint struct{ x, y int32 }

type winMsg struct {
	hwnd     uintptr
	message  uint32
	wParam   uintptr
	lParam   uintptr
	time     uint32
	pt       winPoint
	lPrivate uint32
}

type notifyIconData struct {
	cbSize           uint32
	hWnd             uintptr
	uID              uint32
	uFlags           uint32
	uCallbackMessage uint32
	hIcon            uintptr
	szTip            [128]uint16
	dwState          uint32
	dwStateMask      uint32
	szInfo           [256]uint16
	uVersion         uint32
	szInfoTitle      [64]uint16
	dwInfoFlags      uint32
	guidItem         [16]byte
	hBalloonIcon     uintptr
}

type trayState struct {
	mu             sync.Mutex
	hwnd           uintptr
	icon           uintptr
	actions        trayActions
	taskbarCreated uint32
	pendingTip     string
	pendingInfo    [][2]string
	lastOpen       time.Time
}

var tray = &trayState{}

func copyUTF16(dst []uint16, s string) {
	src, _ := syscall.UTF16FromString(s)
	if len(src) > len(dst) {
		src = src[:len(dst)]
		src[len(src)-1] = 0
	}
	copy(dst, src)
}

func (t *trayState) nid() *notifyIconData {
	n := &notifyIconData{hWnd: t.hwnd, uID: 1}
	n.cbSize = uint32(unsafe.Sizeof(*n))
	return n
}

func (t *trayState) addIcon() {
	n := t.nid()
	n.uFlags = nifMessage | nifIcon | nifTip
	n.uCallbackMessage = wmTray
	n.hIcon = t.icon
	copyUTF16(n.szTip[:], appName+" Download Manager")
	procShellNotifyIconW.Call(nimAdd, uintptr(unsafe.Pointer(n)))
}

func loadTrayIcon(hInst uintptr) uintptr {
	cx, _, _ := procGetSystemMetrics.Call(49) // SM_CXSMICON
	cy, _, _ := procGetSystemMetrics.Call(50)
	// icon embedded in the .exe by rsrc (group id 2 after the manifest, or 1)
	for _, id := range []uintptr{2, 1} {
		if h, _, _ := procLoadImageW.Call(hInst, id, 1 /* IMAGE_ICON */, cx, cy, 0); h != 0 {
			return h
		}
	}
	if len(trayPNG) > 0 {
		if h, _, _ := procCreateIconFromResourceEx.Call(uintptr(unsafe.Pointer(&trayPNG[0])),
			uintptr(len(trayPNG)), 1, 0x00030000, cx, cy, 0); h != 0 {
			return h
		}
	}
	h, _, _ := procLoadIconW.Call(0, 32512 /* IDI_APPLICATION */)
	return h
}

func trayWndProc(hwnd, msg, wParam, lParam uintptr) uintptr {
	t := tray
	switch uint32(msg) {
	case wmTray:
		switch uint32(lParam) & 0xFFFF {
		case wmLButtonUp, wmLButtonDblClk:
			t.open()
		case wmRButtonUp:
			t.showMenu()
		}
		return 0
	case wmTrayUpdate:
		t.flush()
		return 0
	case wmClose:
		procShellNotifyIconW.Call(nimDelete, uintptr(unsafe.Pointer(t.nid())))
		procDestroyWindow.Call(hwnd)
		return 0
	case wmDestroy:
		procPostQuitMessage.Call(0)
		return 0
	}
	if t.taskbarCreated != 0 && uint32(msg) == t.taskbarCreated {
		t.addIcon() // Explorer restarted
		return 0
	}
	r, _, _ := procDefWindowProcW.Call(hwnd, msg, wParam, lParam)
	return r
}

func (t *trayState) open() {
	t.mu.Lock()
	if time.Since(t.lastOpen) < 1500*time.Millisecond {
		t.mu.Unlock()
		return
	}
	t.lastOpen = time.Now()
	t.mu.Unlock()
	go t.actions.Open()
}

func (t *trayState) showMenu() {
	menu, _, _ := procCreatePopupMenu.Call()
	if menu == 0 {
		return
	}
	defer procDestroyMenu.Call(menu)
	add := func(id uintptr, text string) {
		procAppendMenuW.Call(menu, mfString, id, uintptr(unsafe.Pointer(u16(text))))
	}
	add(menuOpen, "Open "+appName)
	procAppendMenuW.Call(menu, mfSeparator, 0, 0)
	add(menuResume, "Resume all")
	add(menuPause, "Pause all")
	procAppendMenuW.Call(menu, mfSeparator, 0, 0)
	add(menuQuit, "Exit")
	procSetMenuDefaultItem.Call(menu, menuOpen, 0)

	var pt winPoint
	procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	procSetForegroundWindow.Call(t.hwnd)
	cmd, _, _ := procTrackPopupMenu.Call(menu, tpmRightButton|tpmBottomAlign|tpmReturnCmd,
		uintptr(pt.x), uintptr(pt.y), 0, t.hwnd, 0)
	procPostMessageW.Call(t.hwnd, wmNull, 0, 0)
	switch cmd {
	case menuOpen:
		t.mu.Lock()
		t.lastOpen = time.Time{}
		t.mu.Unlock()
		t.open()
	case menuResume:
		go t.actions.ResumeAll()
	case menuPause:
		go t.actions.PauseAll()
	case menuQuit:
		go t.actions.Quit()
	}
}

func (t *trayState) flush() {
	t.mu.Lock()
	tip, infos := t.pendingTip, t.pendingInfo
	t.pendingTip, t.pendingInfo = "", nil
	t.mu.Unlock()
	if tip != "" {
		n := t.nid()
		n.uFlags = nifTip
		copyUTF16(n.szTip[:], tip)
		procShellNotifyIconW.Call(nimModify, uintptr(unsafe.Pointer(n)))
	}
	for _, in := range infos {
		n := t.nid()
		n.uFlags = nifInfo
		n.dwInfoFlags = niifInfo
		copyUTF16(n.szInfoTitle[:], in[0])
		copyUTF16(n.szInfo[:], in[1])
		procShellNotifyIconW.Call(nimModify, uintptr(unsafe.Pointer(n)))
	}
}

func (t *trayState) post() {
	t.mu.Lock()
	h := t.hwnd
	t.mu.Unlock()
	if h != 0 {
		procPostMessageW.Call(h, wmTrayUpdate, 0, 0)
	}
}

// runTray creates the notification-area icon and runs its message loop.
func runTray(a trayActions) bool {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	t := tray
	t.actions = a
	hInst, _, _ := procGetModuleHandleW.Call(0)
	className := u16("OpenDMTrayWindow")
	wc := wndClassExW{
		lpfnWndProc:   syscall.NewCallback(trayWndProc),
		hInstance:     hInst,
		lpszClassName: className,
	}
	wc.cbSize = uint32(unsafe.Sizeof(wc))
	if r, _, err := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc))); r == 0 {
		log.Printf("tray: RegisterClassEx failed: %v", err)
		return false
	}
	hwnd, _, err := procCreateWindowExW.Call(0, uintptr(unsafe.Pointer(className)),
		uintptr(unsafe.Pointer(u16(appName))), 0, 0, 0, 0, 0, 0, 0, hInst, 0)
	if hwnd == 0 {
		log.Printf("tray: CreateWindowEx failed: %v", err)
		return false
	}
	tb, _, _ := procRegisterWindowMessageW.Call(uintptr(unsafe.Pointer(u16("TaskbarCreated"))))
	t.mu.Lock()
	t.hwnd = hwnd
	t.taskbarCreated = uint32(tb)
	t.icon = loadTrayIcon(hInst)
	t.mu.Unlock()
	t.addIcon()

	var m winMsg
	for {
		r, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(r) <= 0 {
			break
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		procDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
	}
	return true
}

func trayNotify(title, msg string) {
	tray.mu.Lock()
	tray.pendingInfo = append(tray.pendingInfo, [2]string{title, msg})
	tray.mu.Unlock()
	tray.post()
}

func traySetTip(tip string) {
	tray.mu.Lock()
	tray.pendingTip = tip
	tray.mu.Unlock()
	tray.post()
}

func trayClose() {
	tray.mu.Lock()
	h := tray.hwnd
	tray.mu.Unlock()
	if h != 0 {
		procPostMessageW.Call(h, wmClose, 0, 0)
	}
}
