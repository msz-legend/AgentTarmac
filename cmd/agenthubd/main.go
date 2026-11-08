// Command agenthubd runs the AgentHub daemon: it hosts the ACP bridge on
// 127.0.0.1:8799 (Bearer auth), registers the Claude Code and Codex runtime
// adapters, serves the embedded Web console and owns the SQLite store
// (WAL, retention per decision G).
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"
	"time"

	"github.com/agenttarmac/agenttarmac/internal/acpclient"
	"github.com/agenttarmac/agenttarmac/internal/push"
	"github.com/agenttarmac/agenttarmac/internal/relay"
	"github.com/agenttarmac/agenttarmac/internal/runner"
	"github.com/agenttarmac/agenttarmac/internal/server"
	"github.com/agenttarmac/agenttarmac/internal/tasks"
)

func main() {
	var (
		addr          = flag.String("addr", "127.0.0.1:8799", "listen address (loopback by default)")
		data          = flag.String("data", "data", "data directory for the SQLite database and token file")
		token         = flag.String("token", "", "API bearer token (default: $AGENTHUB_TOKEN or generated into <data>/token)")
		retentionDays = flag.String("retention-days", "90", "session retention in days; 0 = keep forever (PRD D9 default; suggest 90 for a rolling window)")
		hookPath      = flag.String("hook-path", "", "hub-hook binary for managed CC approvals (default: hub-hook(.exe) next to this binary)")
		wecomWebhook  = flag.String("wecom-webhook", "", "WeCom group-bot full webhook URL (default: $AGENTHUB_WECOM_WEBHOOK; PRD M2)")
		wecomBotID    = flag.String("wecom-bot-id", "", "WeCom group-bot id (default: $AGENTHUB_WECOM_BOT_ID)")
		wecomBotToken = flag.String("wecom-bot-token", "", "WeCom group-bot token (used as webhook key; default: $AGENTHUB_WECOM_BOT_TOKEN)")
		taskConc      = flag.Int("task-concurrency", 2, "max concurrently running managed tasks (PRD F6)")
		relayURL      = flag.String("relay", "", "self-hosted relay URL (wss://host/relay/hub/<id>?token=…; PRD D8, default off)")
		feishuWebhook = flag.String("feishu-webhook", "", "Feishu custom-bot webhook URL (default: $AGENTHUB_FEISHU_WEBHOOK)")
		feishuSecret  = flag.String("feishu-secret", "", "Feishu custom-bot sign secret (default: $AGENTHUB_FEISHU_SECRET)")
	)
	flag.Parse()

	logger := log.New(os.Stdout, "agenthubd ", log.LstdFlags|log.Lmsgprefix)

	dbPath := filepath.Join(*data, "agenthub.db")
	db, err := acpclient.InitDB(dbPath)
	if err != nil {
		logger.Fatalf("init database: %v", err)
	}
	defer db.Close()
	store := acpclient.NewStore(db)

	bridge := server.NewBridge(logger)
	bridge.SetStore(store)
	apiToken, err := resolveToken(*token, *data)
	if err != nil {
		logger.Fatalf("resolve api token: %v", err)
	}
	srv := server.NewServer(*addr, bridge, store, apiToken, logger)
	srv.SetRetention(parseDays(*retentionDays))

	// Web Push (PRD F9/M1.5): VAPID keys persist in the data dir so browser
	// subscriptions stay valid across daemon restarts.
	pushMgr, err := push.NewManager(
		filepath.Join(*data, "vapid.json"),
		filepath.Join(*data, "push-subs.json"),
		logger,
	)
	if err != nil {
		logger.Printf("web push disabled: %v", err)
	} else {
		srv.SetPush(pushMgr)
	}

	// Native session resume (PRD F7): attached CC/Codex sessions continue via
	// the vendor CLI; output flows back through the frame pipeline.
	srv.SetRunner(runner.New(runnerSink{bridge: bridge}, logger))

	// F6 task queue (PRD D13): 任务队列 + 看板 + 全托管派发. Managed Claude
	// Code runs carry a settings file routing PreToolUse through hub-hook so
	// approvals land in the same inbox.
	queue := tasks.New(store, bridge, runnerSink{bridge: bridge}, resolveHookCommand(*hookPath), *taskConc)
	srv.SetTasks(queue)
	if hookCmd := resolveHookCommand(*hookPath); hookCmd != "" {
		logger.Printf("task queue ready (concurrency %d, managed CC approvals via hub-hook)", *taskConc)
	} else {
		logger.Printf("task queue ready (concurrency %d; hub-hook not found — managed claude_code tasks will fail fast; build it with go build -o hub-hook.exe ./cmd/hub-hook)", *taskConc)
	}

	// Feishu webhook notifier (decision F).
	webhook := *feishuWebhook
	if webhook == "" {
		webhook = os.Getenv("AGENTHUB_FEISHU_WEBHOOK")
	}
	if webhook != "" {
		secret := *feishuSecret
		if secret == "" {
			secret = os.Getenv("AGENTHUB_FEISHU_SECRET")
		}
		srv.SetNotifier(server.NewFeishuNotifier(webhook, secret, store, logger))
		logger.Printf("feishu webhook notifier enabled")
	}
	wecom := os.Getenv("AGENTHUB_WECOM_WEBHOOK")
	if *wecomWebhook != "" {
		wecom = *wecomWebhook
	} else {
		botID := os.Getenv("AGENTHUB_WECOM_BOT_ID")
		if *wecomBotID != "" {
			botID = *wecomBotID
		}
		tok := os.Getenv("AGENTHUB_WECOM_BOT_TOKEN")
		if *wecomBotToken != "" {
			tok = *wecomBotToken
		}
		if botID != "" || tok != "" {
			// From Bot ID + Token: WeCom group bot send URL is
			// https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=<token>
			wecom = "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=" + tok
		}
	}
	if wecom != "" {
		srv.SetWecomNotifier(server.NewWecomNotifier(wecom, store, logger))
		logger.Printf("wecom webhook notifier enabled")
	}

	// Runtime adapters (decision A): Claude Code + Codex; ACP-native CLIs
	// (OpenCode, Hermes — PRD §7 L1 via ACP) ride the same BaseAdapter with
	// their own kind so approvals, sessions and memory stay kind-scoped.
	register := func(kind string, a server.ACPAdapter) {
		if err := bridge.Register(a); err != nil {
			logger.Fatalf("register %s adapter: %v", kind, err)
		}
	}
	claudeAdapter, err := acpclient.NewClaudeCodeAdapter(store, srv)
	if err != nil {
		logger.Fatalf("create claude code adapter: %v", err)
	}
	register(acpclient.AgentKindClaudeCode, claudeAdapter)
	codexAdapter, err := acpclient.NewCodexAdapter(store, srv)
	if err != nil {
		logger.Fatalf("create codex adapter: %v", err)
	}
	register(acpclient.AgentKindCodex, codexAdapter)
	for _, kind := range []string{acpclient.AgentKindOpenCode, acpclient.AgentKindHermes} {
		a, err := acpclient.NewBaseAdapter(kind, store, srv)
		if err != nil {
			logger.Fatalf("create %s adapter: %v", kind, err)
		}
		register(kind, a)
	}
	for _, a := range []server.ACPAdapter{claudeAdapter, codexAdapter} {
		if err := a.Initialize(context.Background()); err != nil {
			logger.Fatalf("initialize %s adapter: %v", a.AgentKind(), err)
		}
	}

	// Retention sweep (decision G: 默认保留 90 天).
	retention := parseDays(*retentionDays)
	if retention > 0 {
		sweep := func() {
			cutoff := time.Now().AddDate(0, 0, -retention).Unix()
			n, err := store.PurgeSessionsBefore(context.Background(), cutoff)
			if err != nil {
				logger.Printf("retention sweep failed: %v", err)
				return
			}
			if n > 0 {
				logger.Printf("retention sweep: purged %d sessions older than %d days", n, retention)
				// PRD 5.6: the deletion itself is recorded (删除动作留痕).
				_, _ = store.InsertNotificationWithDedup(context.Background(), &acpclient.Notification{
					NotificationType: "system",
					Title:            "保留期清理",
					Body:             fmt.Sprintf("已清理 %d 个超过 %d 天未活动的会话（PRD 5.6 滚动窗口）", n, retention),
					TailHash:         fmt.Sprintf("retention|%s|%d", time.Now().Format("2006-01-02"), n),
				})
			}
		}
		sweep()
		go func() {
			ticker := time.NewTicker(24 * time.Hour)
			defer ticker.Stop()
			for range ticker.C {
				sweep()
			}
		}()
		logger.Printf("retention policy: %d days", retention)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Public relay (PRD D8): Hub dials OUT; off by default (§9 red line 1:
	// 默认关闭、显式开启).
	if *relayURL != "" {
		dialer := &relay.Dialer{RelayURL: *relayURL, Handler: srv.Handler(), Log: logger}
		go func() {
			if err := dialer.Run(ctx); err != nil && ctx.Err() == nil {
				logger.Printf("relay: %v", err)
			}
		}()
		logger.Printf("relay link enabled: %s", *relayURL)
	}

	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()

	logger.Printf("AgentHub daemon listening on http://%s (agent kinds: %v)", srv.Addr(), bridge.AgentKinds())
	logger.Printf("Web console: http://%s/", srv.Addr())
	logger.Printf("API token: %s", tokenHint(*token, *data))

	select {
	case err := <-errCh:
		logger.Fatalf("server: %v", err)
	case <-ctx.Done():
		logger.Printf("shutting down...")
		if err := srv.Shutdown(); err != nil {
			logger.Printf("shutdown error: %v", err)
		}
		_ = claudeAdapter.Finalize()
		_ = codexAdapter.Finalize()
	}
}

func parseDays(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return 90
	}
	return n
}

// resolveToken uses the CLI flag, then $AGENTHUB_TOKEN, then generates a token
// and persists it to <data>/token for Hub clients to pick up.
func resolveToken(flagToken, dataDir string) (string, error) {
	if flagToken != "" {
		return flagToken, nil
	}
	if env := os.Getenv("AGENTHUB_TOKEN"); env != "" {
		return env, nil
	}
	tokenPath := filepath.Join(dataDir, "token")
	if raw, err := os.ReadFile(tokenPath); err == nil && len(raw) >= 32 {
		// Existing token: re-assert the Windows ACL each boot so a file that
		// was created before this hardening (or chmod'd away) gets locked down.
		if runtime.GOOS == "windows" {
			if err := restrictTokenFile(tokenPath); err != nil {
				log.Printf("token file ACL warning: %v", err)
			}
		}
		return string(trimSpaceBytes(raw)), nil
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	tok := hex.EncodeToString(buf)
	if err := os.WriteFile(tokenPath, []byte(tok), 0o600); err != nil {
		return "", err
	}
	if runtime.GOOS == "windows" {
		if err := restrictTokenFile(tokenPath); err != nil {
			log.Printf("token file ACL warning: %v", err)
		}
	}
	return tok, nil
}

func trimSpaceBytes(b []byte) []byte {
	start, end := 0, len(b)
	for start < end && (b[start] == ' ' || b[start] == '\n' || b[start] == '\r' || b[start] == '\t') {
		start++
	}
	for end > start && (b[end-1] == ' ' || b[end-1] == '\n' || b[end-1] == '\r' || b[end-1] == '\t') {
		end--
	}
	return b[start:end]
}

// tokenHint avoids printing the full secret: first run prints the file path.
func tokenHint(flagToken, dataDir string) string {
	if os.Getenv("AGENTHUB_TOKEN") != "" {
		return "from $AGENTHUB_TOKEN"
	}
	return fmt.Sprintf("stored at %s (or pass -token)", filepath.Join(dataDir, "token"))
}

// runnerSink adapts the bridge to the runner's FrameSink interface.
type runnerSink struct{ bridge *server.Bridge }

func (s runnerSink) Dispatch(agentKind, method string, params json.RawMessage) error {
	_, err := s.bridge.DispatchFrame(server.InboundFrame{AgentKind: agentKind, Method: method, Params: params})
	return err
}

// resolveHookCommand finds the hub-hook binary for managed CC approvals.
// Empty = not found (managed claude_code tasks then fail fast with a clear
// error instead of running ungated). The token placeholder is substituted at
// task start by the settings file builder — v1 keeps it simple by baking the
// daemon's own token discovery into the hook via AGENTHUB_TOKEN set here.
func resolveHookCommand(flagPath string) string {
	candidates := []string{}
	if flagPath != "" {
		candidates = append(candidates, flagPath)
	}
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		ext := ""
		if runtime.GOOS == "windows" {
			ext = ".exe"
		}
		candidates = append(candidates, filepath.Join(dir, "hub-hook"+ext))
	}
	for _, c := range candidates {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return fmt.Sprintf("%q", c)
		}
	}
	return ""
}
