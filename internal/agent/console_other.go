//go:build !windows

package agent

func SetConsoleTitle(string) {}

func StartedFromExplorer() bool { return false }
