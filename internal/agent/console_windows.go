//go:build windows

package agent

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	kernel32               = windows.NewLazySystemDLL("kernel32.dll")
	pSetConsoleTitleW      = kernel32.NewProc("SetConsoleTitleW")
	pGetConsoleProcessList = kernel32.NewProc("GetConsoleProcessList")
)

func SetConsoleTitle(title string) {
	p, err := windows.UTF16PtrFromString(title)
	if err == nil {
		pSetConsoleTitleW.Call(uintptr(unsafe.Pointer(p)))
	}
}

// StartedFromExplorer reports whether this process owns its console alone,
// which is the case when it was double-clicked rather than run from a shell.
func StartedFromExplorer() bool {
	var ids [4]uint32
	n, _, _ := pGetConsoleProcessList.Call(uintptr(unsafe.Pointer(&ids[0])), 4)
	return n == 1
}
