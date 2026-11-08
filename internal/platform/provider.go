// Package platform isolates every OS-specific capability the Hub needs
// (PRD §9.1): business code depends only on Provider and never branches on
// runtime.GOOS. The M1 capability set is OpenSessionTerminal (F8 一键拉起
// 系统终端); process discovery / autostart / native notify follow in M2.
package platform

import (
	"context"
	"errors"
)

// ErrUnsupported marks a capability the host OS cannot provide.
var ErrUnsupported = errors.New("platform: capability unsupported on this OS")

// TerminalRequest describes one "open a system terminal here" action
// (PRD F8 / D15: wt.exe -d <cwd> <agent-cmd> <resume-args>).
type TerminalRequest struct {
	CWD     string // working directory (session cwd — CC resume depends on it)
	Command string // full command line to run in the terminal
}

// Provider is the platform capability surface (design/PLATFORM_PROVIDER_v0.1).
type Provider interface {
	// Name reports the OS label used in diagnostics.
	Name() string

	// OpenSessionTerminal launches the OS terminal in the session's cwd
	// running Command. Windows: wt.exe (fallback conhost/cmd start);
	// macOS: Terminal via osascript; Linux: x-terminal-emulator.
	OpenSessionTerminal(ctx context.Context, req TerminalRequest) error
}

// New returns the provider for the current OS.
func New() Provider {
	return newOSProvider()
}
