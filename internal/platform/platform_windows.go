//go:build windows

package platform

import (
	"context"
	"fmt"
	"os/exec"
)

// windowsProvider implements Provider on Windows (PRD D5: Windows 一等公民).
type windowsProvider struct{}

func newOSProvider() Provider { return windowsProvider{} }

// Name reports the OS label.
func (windowsProvider) Name() string { return "windows" }

// OpenSessionTerminal launches Windows Terminal in req.CWD running the
// session resume command; when wt.exe is missing it falls back to
// `cmd /c start` (PRD F8: 本机实测 wt.exe 存在，缺失回退 conhost/cmd start).
func (windowsProvider) OpenSessionTerminal(ctx context.Context, req TerminalRequest) error {
	if req.Command == "" {
		return fmt.Errorf("platform: empty terminal command")
	}
	if wt, err := exec.LookPath("wt.exe"); err == nil {
		// wt.exe -d <cwd> cmd /c <command...>
		args := []string{"-d", req.CWD, "cmd", "/c", req.Command}
		cmd := exec.CommandContext(ctx, wt, args...)
		if err := cmd.Start(); err != nil {
			return fmt.Errorf("platform: wt.exe start: %w", err)
		}
		return nil // wt.exe spawns its own window; do not wait
	}
	// Fallback: a new cmd window keeps running after the command exits (/K).
	cmd := exec.CommandContext(ctx, "cmd", "/c", "start", "", "cmd", "/K", req.Command)
	cmd.Dir = req.CWD
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("platform: cmd start: %w", err)
	}
	return nil
}
