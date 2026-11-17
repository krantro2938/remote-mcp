//go:build windows

package agent

import (
	"os/exec"
	"strconv"
	"syscall"
)

const createNoWindow = 0x08000000

func hideWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
}

// killTree kills the process and all its children (vivado.exe spawns java etc.).
func killTree(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	k := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid))
	hideWindow(k)
	if err := k.Run(); err != nil {
		_ = cmd.Process.Kill()
	}
}
