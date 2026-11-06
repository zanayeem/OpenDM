//go:build windows

package main

import (
	"encoding/base64"
	"errors"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"unicode/utf16"
	"unsafe"
)

var (
	user32   = syscall.NewLazyDLL("user32.dll")
	shell32  = syscall.NewLazyDLL("shell32.dll")
	kernel32 = syscall.NewLazyDLL("kernel32.dll")
	advapi32 = syscall.NewLazyDLL("advapi32.dll")

	procShellExecuteW            = shell32.NewProc("ShellExecuteW")
	procMessageBoxW              = user32.NewProc("MessageBoxW")
	procEnumWindows              = user32.NewProc("EnumWindows")
	procGetWindowTextW           = user32.NewProc("GetWindowTextW")
	procGetClassNameW            = user32.NewProc("GetClassNameW")
	procIsWindowVisible          = user32.NewProc("IsWindowVisible")
	procIsIconic                 = user32.NewProc("IsIconic")
	procShowWindow               = user32.NewProc("ShowWindow")
	procSetForegroundWindow      = user32.NewProc("SetForegroundWindow")
	procAllowSetForegroundWindow = user32.NewProc("AllowSetForegroundWindow")
	procRegCreateKeyExW          = advapi32.NewProc("RegCreateKeyExW")
	procRegSetValueExW           = advapi32.NewProc("RegSetValueExW")
	procRegDeleteValueW          = advapi32.NewProc("RegDeleteValueW")
	procRegCloseKey              = advapi32.NewProc("RegCloseKey")
)

const createNoWindow = 0x08000000

func platformName() string { return "windows" }

func u16(s string) *uint16 {
	p, _ := syscall.UTF16PtrFromString(s)
	return p
}

func shellOpen(target string) error {
	r, _, _ := procShellExecuteW.Call(0, uintptr(unsafe.Pointer(u16("open"))),
		uintptr(unsafe.Pointer(u16(target))), 0, 0, 1 /* SW_SHOWNORMAL */)
	if r <= 32 {
		return errors.New("could not open " + target)
	}
	return nil
}

func openPath(p string) error { return shellOpen(p) }

func revealFile(p string) error {
	cmd := exec.Command("explorer.exe")
	cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: `explorer.exe /select,"` + p + `"`}
	return cmd.Start()
}

// ---------------------------------------------------------------- UI window

var (
	enumOnce   sync.Once
	enumCB     uintptr
	enumMu     sync.Mutex
	enumResult uintptr
)

func windowText(hwnd uintptr, proc *syscall.LazyProc) string {
	buf := make([]uint16, 256)
	n, _, _ := proc.Call(hwnd, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	return syscall.UTF16ToString(buf[:n])
}

// focusExistingWindow brings an already-open OpenDM window to the front.
func focusExistingWindow() bool {
	enumOnce.Do(func() {
		enumCB = syscall.NewCallback(func(hwnd, _ uintptr) uintptr {
			if v, _, _ := procIsWindowVisible.Call(hwnd); v == 0 {
				return 1
			}
			if !strings.HasPrefix(windowText(hwnd, procGetWindowTextW), appName) {
				return 1
			}
			if !strings.HasPrefix(windowText(hwnd, procGetClassNameW), "Chrome_WidgetWin") {
				return 1
			}
			enumResult = hwnd
			return 0
		})
	})
	enumMu.Lock()
	defer enumMu.Unlock()
	enumResult = 0
	procEnumWindows.Call(enumCB, 0)
	if enumResult == 0 {
		return false
	}
	if ic, _, _ := procIsIconic.Call(enumResult); ic != 0 {
		procShowWindow.Call(enumResult, 9 /* SW_RESTORE */)
	}
	procSetForegroundWindow.Call(enumResult)
	return true
}

func findAppBrowser() string {
	var cands []string
	for _, env := range []string{"ProgramFiles(x86)", "ProgramFiles", "LocalAppData"} {
		base := os.Getenv(env)
		if base == "" {
			continue
		}
		cands = append(cands,
			filepath.Join(base, `Microsoft\Edge\Application\msedge.exe`),
			filepath.Join(base, `Google\Chrome\Application\chrome.exe`),
			filepath.Join(base, `BraveSoftware\Brave-Browser\Application\brave.exe`),
		)
	}
	for _, c := range cands {
		if exists(c) {
			return c
		}
	}
	return ""
}

func openUI(u string, appWindow bool) {
	if focusExistingWindow() {
		return
	}
	if appWindow {
		if exe := findAppBrowser(); exe != "" {
			if err := exec.Command(exe, "--app="+u, "--window-size=1220,780").Start(); err == nil {
				return
			}
		}
	}
	if err := shellOpen(u); err != nil {
		log.Printf("open UI: %v", err)
	}
}

func allowForeground() { procAllowSetForegroundWindow.Call(uintptr(0xFFFFFFFF)) }

// ---------------------------------------------------------------- dialogs

func fatalBox(msg string) {
	log.Println(msg)
	procMessageBoxW.Call(0, uintptr(unsafe.Pointer(u16(msg))), uintptr(unsafe.Pointer(u16(appName))), 0x10|0x40000)
}

const folderScript = `
Add-Type -AssemblyName System.Windows.Forms
[Console]::OutputEncoding = [System.Text.Encoding]::UTF8
$owner = New-Object System.Windows.Forms.Form
$owner.TopMost = $true
$d = New-Object System.Windows.Forms.FolderBrowserDialog
$d.Description = 'Choose where OpenDM saves downloads'
$d.ShowNewFolderButton = $true
if ($env:OPENDM_START -and (Test-Path $env:OPENDM_START)) { $d.SelectedPath = $env:OPENDM_START }
if ($d.ShowDialog($owner) -eq [System.Windows.Forms.DialogResult]::OK) { [Console]::Out.Write($d.SelectedPath) }
`

func pickFolder(start string) (string, error) {
	enc := utf16.Encode([]rune(folderScript))
	b := make([]byte, len(enc)*2)
	for i, c := range enc {
		b[2*i], b[2*i+1] = byte(c), byte(c>>8)
	}
	cmd := exec.Command("powershell.exe", "-NoProfile", "-STA", "-NonInteractive",
		"-ExecutionPolicy", "Bypass", "-EncodedCommand", base64.StdEncoding.EncodeToString(b))
	cmd.Env = append(os.Environ(), "OPENDM_START="+start)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// ---------------------------------------------------------------- autostart

func setAutostart(enable bool) {
	const runKey = `Software\Microsoft\Windows\CurrentVersion\Run`
	var hkey syscall.Handle
	r, _, _ := procRegCreateKeyExW.Call(uintptr(syscall.HKEY_CURRENT_USER), uintptr(unsafe.Pointer(u16(runKey))),
		0, 0, 0, 0x0002 /* KEY_SET_VALUE */, 0, uintptr(unsafe.Pointer(&hkey)), 0)
	if r != 0 {
		log.Printf("autostart: open key failed (%d)", r)
		return
	}
	defer procRegCloseKey.Call(uintptr(hkey))
	if !enable {
		procRegDeleteValueW.Call(uintptr(hkey), uintptr(unsafe.Pointer(u16(appName))))
		return
	}
	exe, err := os.Executable()
	if err != nil {
		return
	}
	val, _ := syscall.UTF16FromString(`"` + exe + `" --background`)
	procRegSetValueExW.Call(uintptr(hkey), uintptr(unsafe.Pointer(u16(appName))), 0, 1, /* REG_SZ */
		uintptr(unsafe.Pointer(&val[0])), uintptr(len(val)*2))
}
