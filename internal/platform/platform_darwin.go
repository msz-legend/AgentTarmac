//go:build darwin

package platform

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

type darwinProvider struct{}

func newOSProvider() Provider { return darwinProvider{} }

// Name reports the OS label.
func (darwinProvider) Name() string { return "darwin" }

// OpenSessionTerminal opens Terminal.app at req.CWD running the command
// (PRD F8: osascript -e 'tell application "Terminal" to do script "<cmd>"').
func (darwinProvider) OpenSessionTerminal(ctx context.Context, req TerminalRequest) error {
	if req.Command == "" {
		return fmt.Errorf("platform: empty terminal command")
	}
	script := fmt.Sprintf(`tell application "Terminal" to do script "cd %s && %s"`,
		strings.ReplaceAll(req.CWD, `"`, `\"`), strings.ReplaceAll(req.Command, `"`, `\"`))
	cmd := exec.CommandContext(ctx, "osascript", "-e", script)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("platform: osascript: %w", err)
	}
	return nil
}
