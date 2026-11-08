package pty

import (
	"context"
	"errors"
	"runtime"
	"testing"
)

func TestNewSessionUnsupportedNonWindows(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("ConPTY supported on Windows")
	}
	_, err := NewSession(context.Background(), "echo hi", 80, 24)
	if err != nil && !errors.Is(err, ErrUnsupported) {
		t.Fatalf("expected ErrUnsupported on non-Windows, got %v", err)
	}
}
