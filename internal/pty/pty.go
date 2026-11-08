// Package pty provides a ConPTY-backed pseudo-terminal on Windows so the
// Hub can embed an interactive shell inside the web console (PRD: xterm.js +
// ConPTY + WebSocket). On non-Windows OSes every function returns
// ErrUnsupported so the build stays portable.
package pty

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/UserExistsError/conpty"
)

// ErrUnsupported is returned when the host OS cannot provide a PTY.
var ErrUnsupported = errors.New("pty: pseudo-terminal not supported on this OS")

// Session is one live PTY session. It owns a conpty.ConPty which is a
// bidirectional ReadWriteCloser (keystrokes in, terminal output out).
type Session struct {
	cpty  *conpty.ConPty
	mu    sync.Mutex
	closed bool
}

// NewSession spawns cmd in a new ConPTY with the given initial size (cols x
// rows). It returns a fully wired Session ready for bidirectional I/O.
func NewSession(ctx context.Context, cmd string, cols, rows int) (*Session, error) {
	p, err := conpty.Start(cmd, conpty.ConPtyDimensions(cols, rows))
	if err != nil {
		if errors.Is(err, conpty.ErrConPtyUnsupported) {
			return nil, ErrUnsupported
		}
		return nil, fmt.Errorf("pty: create: %w", err)
	}
	s := &Session{cpty: p}
	return s, nil
}

// Write sends data to the PTY's stdin (keystrokes from the browser).
func (s *Session) Write(b []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return 0, io.ErrClosedPipe
	}
	return s.cpty.Write(b)
}

// Read reads terminal output from the PTY (to send to the browser).
func (s *Session) Read(b []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return 0, io.ErrClosedPipe
	}
	return s.cpty.Read(b)
}

// Resize changes the PTY window size (triggered by browser resize).
func (s *Session) Resize(cols, rows int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return io.ErrClosedPipe
	}
	return s.cpty.Resize(cols, rows)
}

// Close shuts down the PTY and kills the child process.
func (s *Session) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()
	return s.cpty.Close()
}

// Pump copies data between the PTY and a WebSocket connection until either
// side closes or ctx is cancelled. reader/writer are the WS directions.
func Pump(ctx context.Context, s *Session, writeToWS func([]byte) error, readFromWS func() ([]byte, error)) error {
	type result struct{ err error }
	done := make(chan result, 2)

	// PTY -> WebSocket
	go func() {
		buf := make([]byte, 4*1024)
		for {
			n, err := s.Read(buf)
			if n > 0 {
				if e := writeToWS(buf[:n]); e != nil {
					done <- result{err: e}
					return
				}
			}
			if err != nil {
				if err == io.EOF {
					done <- result{err: nil}
				} else {
					done <- result{err: err}
				}
				return
			}
		}
	}()

	// WebSocket -> PTY
	go func() {
		for {
			select {
			case <-ctx.Done():
				done <- result{err: ctx.Err()}
				return
			default:
			}
			b, err := readFromWS()
			if err != nil {
				done <- result{err: err}
				return
			}
			if len(b) > 0 {
				if _, e := s.Write(b); e != nil {
					done <- result{err: e}
					return
				}
			}
		}
	}()

	r := <-done
	_ = s.Close()
	return r.err
}
