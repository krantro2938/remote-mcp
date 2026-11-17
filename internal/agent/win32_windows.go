//go:build windows

package agent

import (
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Raw Win32 bindings. Everything is loaded lazily from system DLLs, so the
// binary needs no CGO and cross-compiles from Linux/macOS.

var (
	user32 = windows.NewLazySystemDLL("user32.dll")
	gdi32  = windows.NewLazySystemDLL("gdi32.dll")
	dwmapi = windows.NewLazySystemDLL("dwmapi.dll")

	pSetProcessDpiAwarenessContext = user32.NewProc("SetProcessDpiAwarenessContext")
	pSetProcessDPIAware            = user32.NewProc("SetProcessDPIAware")
	pGetSystemMetrics              = user32.NewProc("GetSystemMetrics")
	pGetDC                         = user32.NewProc("GetDC")
	pReleaseDC                     = user32.NewProc("ReleaseDC")
	pPrintWindow                   = user32.NewProc("PrintWindow")
	pEnumWindows                   = user32.NewProc("EnumWindows")
	pGetWindowTextW                = user32.NewProc("GetWindowTextW")
	pGetWindowTextLengthW          = user32.NewProc("GetWindowTextLengthW")
	pIsWindowVisible               = user32.NewProc("IsWindowVisible")
	pIsWindow                      = user32.NewProc("IsWindow")
	pIsIconic                      = user32.NewProc("IsIconic")
	pIsZoomed                      = user32.NewProc("IsZoomed")
	pGetWindowThreadProcessId      = user32.NewProc("GetWindowThreadProcessId")
	pGetWindowRect                 = user32.NewProc("GetWindowRect")
	pGetForegroundWindow           = user32.NewProc("GetForegroundWindow")
	pSetForegroundWindow           = user32.NewProc("SetForegroundWindow")
	pBringWindowToTop              = user32.NewProc("BringWindowToTop")
	pShowWindow                    = user32.NewProc("ShowWindow")
	pMoveWindow                    = user32.NewProc("MoveWindow")
	pPostMessageW                  = user32.NewProc("PostMessageW")
	pSetCursorPos                  = user32.NewProc("SetCursorPos")
	pGetCursorPos                  = user32.NewProc("GetCursorPos")
	pSendInput                     = user32.NewProc("SendInput")
	pMapVirtualKeyW                = user32.NewProc("MapVirtualKeyW")
	pVkKeyScanW                    = user32.NewProc("VkKeyScanW")
	pEnumDisplayMonitors           = user32.NewProc("EnumDisplayMonitors")
	pGetMonitorInfoW               = user32.NewProc("GetMonitorInfoW")

	pCreateCompatibleDC     = gdi32.NewProc("CreateCompatibleDC")
	pCreateCompatibleBitmap = gdi32.NewProc("CreateCompatibleBitmap")
	pSelectObject           = gdi32.NewProc("SelectObject")
	pBitBlt                 = gdi32.NewProc("BitBlt")
	pDeleteDC               = gdi32.NewProc("DeleteDC")
	pDeleteObject           = gdi32.NewProc("DeleteObject")
	pGetDIBits              = gdi32.NewProc("GetDIBits")

	pDwmGetWindowAttribute = dwmapi.NewProc("DwmGetWindowAttribute")
)

const (
	smXVirtualScreen  = 76
	smYVirtualScreen  = 77
	smCXVirtualScreen = 78
	smCYVirtualScreen = 79

	srcCopy    = 0x00CC0020
	captureBlt = 0x40000000

	pwRenderFullContent = 0x2

	dwmwaExtendedFrameBounds = 9
	dwmwaCloaked             = 14

	swMaximize = 3
	swMinimize = 6
	swRestore  = 9

	wmClose = 0x0010

	inputMouse    = 0
	inputKeyboard = 1

	mouseLeftDown   = 0x0002
	mouseLeftUp     = 0x0004
	mouseRightDown  = 0x0008
	mouseRightUp    = 0x0010
	mouseMiddleDown = 0x0020
	mouseMiddleUp   = 0x0040
	mouseWheel      = 0x0800
	mouseHWheel     = 0x1000

	keyExtended = 0x0001
	keyUp       = 0x0002
	keyUnicode  = 0x0004

	monitorInfoPrimary = 0x1
)

type winRect struct{ Left, Top, Right, Bottom int32 }

type winPoint struct{ X, Y int32 }

type bitmapInfoHeader struct {
	Size          uint32
	Width         int32
	Height        int32
	Planes        uint16
	BitCount      uint16
	Compression   uint32
	SizeImage     uint32
	XPelsPerMeter int32
	YPelsPerMeter int32
	ClrUsed       uint32
	ClrImportant  uint32
}

type bitmapInfo struct {
	Header bitmapInfoHeader
	Colors [1]uint32
}

// INPUT layout for 64-bit Windows (amd64/arm64): 4-byte type, 4 bytes padding,
// then a 32-byte union. We only build 64-bit agents.
type mouseInput struct {
	Dx, Dy    int32
	MouseData uint32
	Flags     uint32
	Time      uint32
	ExtraInfo uintptr
}

type keybdInput struct {
	Vk        uint16
	Scan      uint16
	Flags     uint32
	Time      uint32
	ExtraInfo uintptr
	_         [8]byte // pad to the size of the union
}

type mouseINPUT struct {
	Type uint32
	_    uint32
	Mi   mouseInput
}

type keybdINPUT struct {
	Type uint32
	_    uint32
	Ki   keybdInput
}

type monitorInfo struct {
	Size    uint32
	Monitor winRect
	Work    winRect
	Flags   uint32
}

func boolCall(p *windows.LazyProc, args ...uintptr) bool {
	r, _, _ := p.Call(args...)
	return r != 0
}

func getSystemMetrics(i int) int {
	r, _, _ := pGetSystemMetrics.Call(uintptr(i))
	return int(int32(r))
}

func windowText(h uintptr) string {
	n, _, _ := pGetWindowTextLengthW.Call(h)
	if n == 0 {
		return ""
	}
	buf := make([]uint16, n+1)
	pGetWindowTextW.Call(h, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	return syscall.UTF16ToString(buf)
}

func windowRect(h uintptr) (winRect, bool) {
	var r winRect
	ok := boolCall(pGetWindowRect, h, uintptr(unsafe.Pointer(&r)))
	return r, ok
}

// frameBounds returns the visible window bounds, without the invisible resize
// borders Windows 10/11 adds around every window.
func frameBounds(h uintptr) (winRect, bool) {
	var r winRect
	if pDwmGetWindowAttribute.Find() == nil {
		hr, _, _ := pDwmGetWindowAttribute.Call(h, dwmwaExtendedFrameBounds, uintptr(unsafe.Pointer(&r)), unsafe.Sizeof(r))
		if hr == 0 && r.Right > r.Left {
			return r, true
		}
	}
	return windowRect(h)
}

func isCloaked(h uintptr) bool {
	if pDwmGetWindowAttribute.Find() != nil {
		return false
	}
	var cloaked uint32
	hr, _, _ := pDwmGetWindowAttribute.Call(h, dwmwaCloaked, uintptr(unsafe.Pointer(&cloaked)), 4)
	return hr == 0 && cloaked != 0
}

func windowPID(h uintptr) uint32 {
	var pid uint32
	pGetWindowThreadProcessId.Call(h, uintptr(unsafe.Pointer(&pid)))
	return pid
}

func processName(pid uint32) string {
	ph, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return ""
	}
	defer windows.CloseHandle(ph)
	buf := make([]uint16, 1024)
	size := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(ph, 0, &buf[0], &size); err != nil {
		return ""
	}
	full := syscall.UTF16ToString(buf[:size])
	for i := len(full) - 1; i >= 0; i-- {
		if full[i] == '\\' {
			return full[i+1:]
		}
	}
	return full
}

func sendInputs(ptr unsafe.Pointer, n int, size uintptr) error {
	r, _, err := pSendInput.Call(uintptr(n), uintptr(ptr), size)
	if int(r) != n {
		return err
	}
	return nil
}
