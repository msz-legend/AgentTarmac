// Command agenthub-connect bridges one real ACP agent subprocess (JSON-RPC
// over stdio) to the AgentHub daemon, so its sessions are hosted, gated and
// audited by the Hub.
//
// Spawn an agent:
//
//	agenthub-connect -daemon http://127.0.0.1:8799 \
//	    -command npx --args "-y,@zed-industries/claude-code-acp"
//
// Passthrough mode (wire an external agent's stdio through this process):
//
//	some-acp-agent | agenthub-connect -daemon … | some-acp-agent
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/agenttarmac/agenttarmac/internal/connector"
)

func main() {
	var (
		daemon      = flag.String("daemon", "http://127.0.0.1:8799", "AgentHub daemon base URL")
		token       = flag.String("token", "", "API bearer token (default: $AGENTHUB_TOKEN)")
		agentKind   = flag.String("agent-kind", "claude_code", "agent kind registered at the daemon")
		commandFlag = flag.String("command", "", "agent command to spawn (empty + no args = stdio passthrough)")
	)
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: agenthub-connect [flags] [agent-command [agent-args…]]\n\n"+
			"example: agenthub-connect -token $TOKEN npx -y @zed-industries/claude-code-acp\n\nflags:\n")
		flag.PrintDefaults()
	}
	flag.Parse()
	command := *commandFlag
	agentArgs := []string{}
	if flag.NArg() > 0 {
		command = flag.Arg(0)
		agentArgs = flag.Args()[1:]
	}
	if *token == "" {
		*token = os.Getenv("AGENTHUB_TOKEN")
	}
	if *token == "" {
		fmt.Fprintln(os.Stderr, "agenthub-connect: no token (use -token or $AGENTHUB_TOKEN)")
		os.Exit(2)
	}

	logger := log.New(os.Stderr, "agenthub-connect ", log.LstdFlags|log.Lmsgprefix)

	cwd, _ := os.Getwd()
	cfg := connector.Config{
		DaemonURL: *daemon,
		Token:     *token,
		AgentKind: *agentKind,
		Command:   command,
		Args:      agentArgs,
		CWD:       cwd,
		Log:       logger,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if command == "" {
		// Passthrough: JSON-RPC on our own stdio.
		logger.Printf("passthrough mode → %s (%s)", *daemon, *agentKind)
		if err := connector.New(cfg).Run(ctx, os.Stdout, os.Stdin); err != nil {
			logger.Fatalf("connector: %v", err)
		}
		return
	}

	// Spawn mode: run the agent subprocess and pipe its stdio.
	logger.Printf("spawning %s %s → %s (%s)", command, strings.Join(agentArgs, " "), *daemon, *agentKind)
	cmd := exec.CommandContext(ctx, command, agentArgs...)
	cmd.Stderr = os.Stderr

	stdin, err := cmd.StdinPipe()
	if err != nil {
		logger.Fatalf("stdin pipe: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		logger.Fatalf("stdout pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		logger.Fatalf("spawn agent: %v", err)
	}

	connDone := make(chan error, 1)
	go func() { connDone <- connector.New(cfg).Run(ctx, stdin, stdout) }()

	agentDone := make(chan error, 1)
	go func() { agentDone <- cmd.Wait() }()

	select {
	case err := <-connDone:
		if err != nil && ctx.Err() == nil {
			logger.Printf("connector: %v", err)
		}
		// Agent stdin closed; give the agent a moment to exit on its own.
		if closer, ok := stdin.(io.Closer); ok {
			_ = closer.Close()
		}
		select {
		case <-agentDone:
		case <-ctx.Done():
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
		}
	case err := <-agentDone:
		if err != nil && ctx.Err() == nil {
			logger.Printf("agent exited: %v", err)
		}
	}
}
