//go:build darwin

package osprobe

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"runtime"
	"time"
	"unsafe"

	"github.com/ebitengine/purego"
	"github.com/ebitengine/purego/objc"
)

var _ prober = (*Probe)(nil)

// Framework paths. Since macOS 11 the binaries live in the dyld shared cache,
// and dlopen of these paths still resolves them.
const (
	coreGraphicsPath = "/System/Library/Frameworks/CoreGraphics.framework/CoreGraphics"
	coreFoundPath    = "/System/Library/Frameworks/CoreFoundation.framework/CoreFoundation"
	appKitPath       = "/System/Library/Frameworks/AppKit.framework/AppKit"
	libObjCPath      = "/usr/lib/libobjc.A.dylib"
)

// Apple constants.
const (
	cgEventSourceStateCombinedSession = 0          // kCGEventSourceStateCombinedSessionState
	cgAnyInputEventType               = 0xFFFFFFFF // kCGAnyInputEventType (~0)
	cgWindowListOnScreenOnly          = 1 << 0     // kCGWindowListOptionOnScreenOnly
	cgWindowListExcludeDesktop        = 1 << 4     // kCGWindowListExcludeDesktopElements
	cfNumberSInt32Type                = 3          // kCFNumberSInt32Type
	cfStringEncodingUTF8              = 0x08000100 // kCFStringEncodingUTF8
	maxWindowsScanned                 = 64
)

// Probe reads idle time, the frontmost app and lock state on macOS through
// purego (no cgo; purego's internal fakecgo runtime makes dlopen work with
// CGO_ENABLED=0 on darwin/amd64 and darwin/arm64).
//
// None of these calls trigger a permission prompt:
//   - CGEventSourceSecondsSinceLastEventType needs no Input Monitoring or
//     Accessibility permission (it reads a counter, not events);
//   - CGWindowListCopyWindowInfo returns window owner PIDs and names without
//     Screen Recording permission; only window titles (kCGWindowName) need
//     it, and they are never read;
//   - NSRunningApplication lookups by PID need no permission.
//
// The frontmost app is taken from the window list (front-to-back order, first
// normal-layer window) rather than NSWorkspace.frontmostApplication, because
// NSWorkspace only refreshes that property from main-run-loop notifications,
// which a daemon without an NSApplication run loop never processes.
type Probe struct {
	log     *slog.Logger
	loadErr error

	secondsSinceLastEvent func(state int32, eventType uint32) float64
	windowListCopy        func(option uint32, relativeTo uint32) uintptr
	sessionCopyDict       func() uintptr
	cfArrayGetCount       func(arr uintptr) int
	cfArrayGetValueAt     func(arr uintptr, i int) uintptr
	cfDictGetValue        func(dict uintptr, key uintptr) uintptr
	cfNumberGetValue      func(num uintptr, typ int32, out unsafe.Pointer) bool
	cfBooleanGetValue     func(b uintptr) bool
	cfStringCreate        func(alloc uintptr, s string, enc uint32) uintptr
	cfStringGetCString    func(s uintptr, buf *byte, size int, enc uint32) bool
	cfRelease             func(obj uintptr)

	keyLayer, keyOwnerPID, keyOwnerName, keyLocked uintptr

	msgSendID     func(id objc.ID, sel objc.SEL) objc.ID
	msgSendPID    func(id objc.ID, sel objc.SEL, pid int32) objc.ID
	msgSendString func(id objc.ID, sel objc.SEL) string
	clsPool       objc.ID
	clsRunningApp objc.ID
	selNew        objc.SEL
	selDrain      objc.SEL
	selAppForPID  objc.SEL
	selBundleID   objc.SEL
	selLocalName  objc.SEL
	selUTF8       objc.SEL
}

// New loads the frameworks. A load failure is remembered and returned by
// every call, so the sensor reports unknown instead of crashing.
func New(log *slog.Logger) *Probe {
	p := &Probe{log: log}
	p.loadErr = p.load()
	if p.loadErr != nil {
		log.Warn("macOS activity APIs unavailable", "err", p.loadErr)
	}
	return p
}

func (p *Probe) load() (err error) {
	defer func() {
		// purego panics when a symbol is missing; turn that into an error.
		if r := recover(); r != nil {
			err = fmt.Errorf("loading macOS symbols: %v", r)
		}
	}()
	open := func(path string) (uintptr, error) {
		h, err := purego.Dlopen(path, purego.RTLD_LAZY|purego.RTLD_GLOBAL)
		if err != nil {
			return 0, fmt.Errorf("dlopen %s: %w", path, err)
		}
		return h, nil
	}
	cg, err := open(coreGraphicsPath)
	if err != nil {
		return err
	}
	cf, err := open(coreFoundPath)
	if err != nil {
		return err
	}
	if _, err := open(appKitPath); err != nil { // registers NSRunningApplication
		return err
	}
	libobjc, err := open(libObjCPath)
	if err != nil {
		return err
	}
	purego.RegisterLibFunc(&p.secondsSinceLastEvent, cg, "CGEventSourceSecondsSinceLastEventType")
	purego.RegisterLibFunc(&p.windowListCopy, cg, "CGWindowListCopyWindowInfo")
	purego.RegisterLibFunc(&p.sessionCopyDict, cg, "CGSessionCopyCurrentDictionary")
	purego.RegisterLibFunc(&p.cfArrayGetCount, cf, "CFArrayGetCount")
	purego.RegisterLibFunc(&p.cfArrayGetValueAt, cf, "CFArrayGetValueAtIndex")
	purego.RegisterLibFunc(&p.cfDictGetValue, cf, "CFDictionaryGetValue")
	purego.RegisterLibFunc(&p.cfNumberGetValue, cf, "CFNumberGetValue")
	purego.RegisterLibFunc(&p.cfBooleanGetValue, cf, "CFBooleanGetValue")
	purego.RegisterLibFunc(&p.cfStringCreate, cf, "CFStringCreateWithCString")
	purego.RegisterLibFunc(&p.cfStringGetCString, cf, "CFStringGetCString")
	purego.RegisterLibFunc(&p.cfRelease, cf, "CFRelease")
	purego.RegisterLibFunc(&p.msgSendID, libobjc, "objc_msgSend")
	purego.RegisterLibFunc(&p.msgSendPID, libobjc, "objc_msgSend")
	purego.RegisterLibFunc(&p.msgSendString, libobjc, "objc_msgSend")

	for name, dst := range map[string]*uintptr{
		"kCGWindowLayer": &p.keyLayer, "kCGWindowOwnerPID": &p.keyOwnerPID, "kCGWindowOwnerName": &p.keyOwnerName,
	} {
		if *dst, err = cfConstant(cg, name); err != nil {
			return err
		}
	}
	// Created once and kept for the process lifetime.
	p.keyLocked = p.cfStringCreate(0, "CGSSessionScreenIsLocked", cfStringEncodingUTF8)
	if p.keyLocked == 0 {
		return errors.New("creating CFString key")
	}

	p.clsPool = objc.ID(objc.GetClass("NSAutoreleasePool"))
	p.clsRunningApp = objc.ID(objc.GetClass("NSRunningApplication"))
	if p.clsPool == 0 || p.clsRunningApp == 0 {
		return errors.New("objective-c classes NSAutoreleasePool/NSRunningApplication not found")
	}
	p.selNew = objc.RegisterName("new")
	p.selDrain = objc.RegisterName("drain")
	p.selAppForPID = objc.RegisterName("runningApplicationWithProcessIdentifier:")
	p.selBundleID = objc.RegisterName("bundleIdentifier")
	p.selLocalName = objc.RegisterName("localizedName")
	p.selUTF8 = objc.RegisterName("UTF8String")
	return nil
}

// cfConstant reads an exported `const CFStringRef` symbol's value.
func cfConstant(lib uintptr, name string) (uintptr, error) {
	sym, err := purego.Dlsym(lib, name)
	if err != nil {
		return 0, fmt.Errorf("dlsym %s: %w", name, err)
	}
	// sym is the address of a CFStringRef variable inside the framework,
	// which stays mapped for the process lifetime.
	v := **(**uintptr)(unsafe.Pointer(&sym)) //nolint:gosec // G103: dereferencing a C global's address returned by dlsym
	if v == 0 {
		return 0, fmt.Errorf("%s is nil", name)
	}
	return v, nil
}

// IdleTime returns seconds since the last input event of any kind in the
// login session (kCGEventSourceStateCombinedSessionState, kCGAnyInputEventType).
func (p *Probe) IdleTime(context.Context) (time.Duration, error) {
	if p.loadErr != nil {
		return 0, p.loadErr
	}
	s := p.secondsSinceLastEvent(cgEventSourceStateCombinedSession, cgAnyInputEventType)
	if math.IsNaN(s) || s < 0 {
		return 0, fmt.Errorf("CGEventSourceSecondsSinceLastEventType returned %v", s)
	}
	return time.Duration(s * float64(time.Second)), nil
}

// FocusedApp returns the bundle id of the app owning the frontmost normal
// window, falling back to its localized name, then the window owner name.
func (p *Probe) FocusedApp(context.Context) (string, error) {
	if p.loadErr != nil {
		return "", p.loadErr
	}
	pid, owner := p.frontmostWindowOwner()
	if pid <= 0 {
		return "", nil
	}
	if name := p.bundleOrName(pid); name != "" {
		return truncateName(name), nil
	}
	return truncateName(owner), nil
}

// frontmostWindowOwner scans the on-screen window list front to back and
// returns the owner PID and owner name of the first layer-0 window.
func (p *Probe) frontmostWindowOwner() (int32, string) {
	list := p.windowListCopy(cgWindowListOnScreenOnly|cgWindowListExcludeDesktop, 0)
	if list == 0 {
		return 0, ""
	}
	defer p.cfRelease(list)
	n := min(p.cfArrayGetCount(list), maxWindowsScanned)
	for i := range n {
		w := p.cfArrayGetValueAt(list, i)
		var layer, pid int32
		if !p.number(w, p.keyLayer, &layer) || layer != 0 {
			continue // menu bar, Dock, overlays
		}
		if !p.number(w, p.keyOwnerPID, &pid) {
			continue
		}
		return pid, p.cfString(p.cfDictGetValue(w, p.keyOwnerName))
	}
	return 0, ""
}

func (p *Probe) number(dict, key uintptr, out *int32) bool {
	v := p.cfDictGetValue(dict, key)
	return v != 0 && p.cfNumberGetValue(v, cfNumberSInt32Type, unsafe.Pointer(out)) //nolint:gosec // G103: C out-parameter sized for kCFNumberSInt32Type
}

func (p *Probe) cfString(s uintptr) string {
	if s == 0 {
		return ""
	}
	var buf [maxAppNameLen + 1]byte
	if !p.cfStringGetCString(s, &buf[0], len(buf), cfStringEncodingUTF8) {
		return ""
	}
	for i, b := range buf {
		if b == 0 {
			return string(buf[:i])
		}
	}
	return ""
}

// bundleOrName asks NSRunningApplication for the PID's bundle id (or name).
// It runs inside an autorelease pool on a locked OS thread, because a
// goroutine's thread has no pool of its own and the autoreleased strings
// would otherwise leak.
func (p *Probe) bundleOrName(pid int32) string {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	pool := p.msgSendID(p.clsPool, p.selNew)
	defer p.msgSendID(pool, p.selDrain)
	app := p.msgSendPID(p.clsRunningApp, p.selAppForPID, pid)
	if app == 0 {
		return ""
	}
	for _, sel := range []objc.SEL{p.selBundleID, p.selLocalName} {
		if s := p.msgSendID(app, sel); s != 0 {
			if str := p.msgSendString(s, p.selUTF8); str != "" {
				return str
			}
		}
	}
	return ""
}

// ScreenLocked reads CGSSessionScreenIsLocked from the session dictionary.
func (p *Probe) ScreenLocked(context.Context) (bool, error) {
	if p.loadErr != nil {
		return false, p.loadErr
	}
	dict := p.sessionCopyDict()
	if dict == 0 {
		return false, fmt.Errorf("no GUI login session: %w", ErrUnavailable)
	}
	defer p.cfRelease(dict)
	v := p.cfDictGetValue(dict, p.keyLocked)
	return v != 0 && p.cfBooleanGetValue(v), nil
}

// Sources describes the backends in use.
func (p *Probe) Sources() map[string]string {
	if p.loadErr != nil {
		return map[string]string{"idle": "unavailable", "focus": "unavailable", "lock": "unavailable"}
	}
	return map[string]string{
		"idle":  "CGEventSourceSecondsSinceLastEventType",
		"focus": "CGWindowListCopyWindowInfo + NSRunningApplication",
		"lock":  "CGSessionCopyCurrentDictionary",
	}
}

// Close is a no-op: frameworks stay loaded for the process lifetime.
func (p *Probe) Close() error { return nil }
