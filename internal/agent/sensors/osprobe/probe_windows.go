//go:build windows

package osprobe

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var _ prober = (*Probe)(nil)

// Win32 constants not exported by x/sys/windows.
const (
	desktopReadObjects = 0x0001 // DESKTOP_READOBJECTS
	uoiName            = 2      // UOI_NAME
)

// lastInputInfo mirrors LASTINPUTINFO.
type lastInputInfo struct {
	cbSize uint32
	dwTime uint32
}

// Probe reads idle time, focus and lock state through user32 and kernel32.
// The DLLs load lazily from System32 only (NewLazySystemDLL), so a planted
// DLL next to the binary is never picked up.
type Probe struct {
	log *slog.Logger

	getLastInputInfo         *windows.LazyProc
	getTickCount64           *windows.LazyProc
	openInputDesktop         *windows.LazyProc
	closeDesktop             *windows.LazyProc
	getUserObjectInformation *windows.LazyProc
}

// New returns a Windows probe.
func New(log *slog.Logger) *Probe {
	user32 := windows.NewLazySystemDLL("user32.dll")
	kernel32 := windows.NewLazySystemDLL("kernel32.dll")
	return &Probe{
		log:                      log,
		getLastInputInfo:         user32.NewProc("GetLastInputInfo"),
		getTickCount64:           kernel32.NewProc("GetTickCount64"),
		openInputDesktop:         user32.NewProc("OpenInputDesktop"),
		closeDesktop:             user32.NewProc("CloseDesktop"),
		getUserObjectInformation: user32.NewProc("GetUserObjectInformationW"),
	}
}

// IdleTime uses GetLastInputInfo, which covers keyboard and mouse input for
// the whole session (not only this process), against GetTickCount64.
func (p *Probe) IdleTime(context.Context) (time.Duration, error) {
	if err := p.getLastInputInfo.Find(); err != nil {
		return 0, fmt.Errorf("GetLastInputInfo: %w", err)
	}
	if err := p.getTickCount64.Find(); err != nil {
		return 0, fmt.Errorf("GetTickCount64: %w", err)
	}
	info := lastInputInfo{cbSize: uint32(unsafe.Sizeof(lastInputInfo{}))}
	//nolint:gosec // G103: passing a pointer to a Win32 out-parameter struct is the documented calling convention
	if r, _, err := p.getLastInputInfo.Call(uintptr(unsafe.Pointer(&info))); r == 0 {
		return 0, fmt.Errorf("GetLastInputInfo: %w", err)
	}
	lo, hi, _ := p.getTickCount64.Call() // cannot fail
	now := uint64(lo)
	if unsafe.Sizeof(uintptr(0)) == 4 {
		now |= uint64(hi) << 32 // 32-bit Windows returns the 64-bit value in EDX:EAX
	}
	return idleFromTicks(now, info.dwTime), nil
}

// FocusedApp returns the foreground window's executable base name, e.g.
// "Code.exe". It opens the process with PROCESS_QUERY_LIMITED_INFORMATION
// only, which needs no admin rights; the window title is never read.
func (p *Probe) FocusedApp(context.Context) (string, error) {
	hwnd := windows.GetForegroundWindow()
	if hwnd == 0 {
		return "", nil // no foreground window, e.g. during a desktop switch
	}
	var pid uint32
	if _, err := windows.GetWindowThreadProcessId(hwnd, &pid); err != nil || pid == 0 {
		return "", nil //nolint:nilerr // the window closed between calls; unknown, not an error
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return "", nil //nolint:nilerr // protected processes refuse even limited queries; report unknown
	}
	defer func() { _ = windows.CloseHandle(h) }() // nothing useful to do if closing fails
	var buf [windows.MAX_LONG_PATH]uint16
	size := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &size); err != nil {
		return "", fmt.Errorf("QueryFullProcessImageName: %w", err)
	}
	return truncateName(filepath.Base(windows.UTF16ToString(buf[:size]))), nil
}

// ScreenLocked reports whether the input desktop is something other than
// "Default": the lock screen and UAC prompts run on the secure "Winlogon"
// desktop, which a normal process cannot open at all.
func (p *Probe) ScreenLocked(context.Context) (bool, error) {
	if err := p.openInputDesktop.Find(); err != nil {
		return false, fmt.Errorf("OpenInputDesktop: %w", err)
	}
	h, _, _ := p.openInputDesktop.Call(0, 0, desktopReadObjects)
	if h == 0 {
		return true, nil // the input desktop is the secure desktop
	}
	defer func() { _, _, _ = p.closeDesktop.Call(h) }() // best effort; the handle is ours
	var name [64]uint16
	var needed uint32
	//nolint:gosec // G103: Win32 out-parameters; the buffer length passed bounds the write
	r, _, err := p.getUserObjectInformation.Call(h, uoiName, uintptr(unsafe.Pointer(&name[0])),
		uintptr(len(name)*2), uintptr(unsafe.Pointer(&needed)))
	if r == 0 {
		return false, fmt.Errorf("GetUserObjectInformationW: %w", err)
	}
	return windows.UTF16ToString(name[:]) != "Default", nil
}

// Sources describes the backends in use.
func (p *Probe) Sources() map[string]string {
	return map[string]string{
		"idle":  "GetLastInputInfo",
		"focus": "GetForegroundWindow + QueryFullProcessImageNameW",
		"lock":  "OpenInputDesktop",
	}
}

// Close is a no-op: lazy DLLs stay loaded for the process lifetime.
func (p *Probe) Close() error { return nil }
