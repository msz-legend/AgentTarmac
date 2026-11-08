//go:build linux

package platform

import (
	"context"
	"fmt"
	"os/exec"
)

type linuxProvider struct{}

func newOSProvider() Provider { return linuxProvider{} }

// Name reports the OS label.
func (linuxProvider) Name() string { return "linux" }

// OpenSessionTerminal opens the system terminal (PRD F8: x-terminal-emulator,
// 按 DE 回退).
func (linuxProvider) OpenSessionTerminal(ctx context.Context, req TerminalRequest) error {
	if req.Command == "" {
		return fmt.Errorf("platform: empty terminal command")
	}
	for _, term := range []string{"x-terminal-emulator", "gnome-terminal", "konsole", "xfce4-terminal"} {
		if path, err := exec.LookPath(term); err == nil {
			cmd := exec.CommandContext(ctx, path, "-e", "sh -c '"+req.Command+"'")
			cmd.Dir = req.CWD
			if err := cmd.Start(); err != nil {
				return fmt.Errorf("platform: %s start: %w", term, err)
			}
			return nil
		}
	}
	return ErrUnsupported
}
