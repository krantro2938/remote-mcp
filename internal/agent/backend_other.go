//go:build !windows

package agent

// NewPlatformBackend on non-Windows systems returns the demo backend, so the
// agent can be run on Linux/macOS to test the relay without a real desktop.
func NewPlatformBackend() Backend { return NewDemoBackend() }
