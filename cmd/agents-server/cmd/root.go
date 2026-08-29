// Package cmd implements the agents-server command-line entry point and server bootstrap.
package cmd

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/zzir/agents-go/cmd/agents-server/internal/bridge"
	"github.com/zzir/agents-go/cmd/agents-server/internal/handler"
	"github.com/zzir/agents-go/cmd/agents-server/internal/logging"
	"github.com/zzir/agents-go/cmd/agents-server/internal/secrets"
	"github.com/zzir/agents-go/cmd/agents-server/internal/server"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

var (
	flagHost           string
	flagPort           int
	flagPreviewPort    int
	flagPreviewBaseURL string
	flagDB             string
	flagToken          string
	flagLogLevel       string
	flagLogFormat      string
	flagBaseURL        string
	flagTrustedProxies string
	flagAuthMode       string
	flagGoogleClientID string
	flagGoogleSecret   string
	flagAllowedDomains string
	flagAllowedEmails  string
	flagBootstrapAdmin string
	flagAuditRetention int
	flagSecretKeyFile  string
)

var rootCmd = &cobra.Command{
	Use:   "agents-go server",
	Short: "The Go-native agent workbench you run yourself",
	RunE:  run,
}

func init() {
	rootCmd.Flags().StringVar(&flagHost, "host", "127.0.0.1", "Bind address (use 0.0.0.0 for LAN access)")
	rootCmd.Flags().IntVar(&flagPort, "port", 9527, "HTTP server port")
	rootCmd.Flags().IntVar(&flagPreviewPort, "preview-port", 0, "Port for the isolated sandbox-preview origin (0 = port+1); must differ from --port so a previewed page cannot read the app's token")
	rootCmd.Flags().StringVar(&flagPreviewBaseURL, "preview-base-url", "", "Public origin the preview listener is reached at, scheme://host[:port] (behind a reverse proxy that routes a second hostname to the preview port)")
	rootCmd.Flags().StringVar(&flagDB, "db", "data.db", "SQLite database path, or a postgres:// DSN")
	rootCmd.Flags().StringVar(&flagToken, "token", "", "Authentication token (or env AGENTS_TOKEN; auto-generated if empty)")
	rootCmd.Flags().StringVar(&flagLogLevel, "log-level", "info", "Log level: debug, info, warn, error")
	rootCmd.Flags().StringVar(&flagLogFormat, "log-format", "text", "Log format: text, json")
	rootCmd.Flags().StringVar(&flagBaseURL, "base-url", "", "Public origin of this server, scheme://host[:port] (required behind a reverse proxy for OAuth flows)")
	rootCmd.Flags().StringVar(&flagTrustedProxies, "trusted-proxies", "", "Comma-separated proxy IPs/CIDRs whose X-Forwarded-For is believed for client IPs (default: none)")
	rootCmd.Flags().StringVar(&flagAuthMode, "auth", "token", "Authentication mode: token (single static token) or oauth (per-user login)")
	rootCmd.Flags().StringVar(&flagGoogleClientID, "oauth-google-client-id", "", "Google OAuth client id (enables the google login provider)")
	rootCmd.Flags().StringVar(&flagGoogleSecret, "oauth-google-client-secret", "", "Google OAuth client secret (or env AGENTS_OAUTH_GOOGLE_CLIENT_SECRET)")
	rootCmd.Flags().StringVar(&flagAllowedDomains, "allowed-domains", "", "Comma-separated email domains admitted to OAuth login")
	rootCmd.Flags().StringVar(&flagAllowedEmails, "allowed-emails", "", "Comma-separated email addresses admitted to OAuth login")
	rootCmd.Flags().StringVar(&flagBootstrapAdmin, "bootstrap-admin", "", "Email that signs in as admin (implicitly admitted; the recovery hatch)")
	rootCmd.Flags().IntVar(&flagAuditRetention, "audit-retention-days", 0, "Prune audit log entries older than this many days (0 = keep forever); a process setting, never an API one")
	rootCmd.Flags().StringVar(&flagSecretKeyFile, "secret-key-file", "", "File holding the 32-byte key (base64 or hex) that seals stored credentials; or env AGENTS_SECRET_KEY")
}

// buildVersion is the plain version string (without commit/date), surfaced by
// the /health endpoint.
var buildVersion = "dev"

// splitList parses a comma-separated flag into trimmed, non-empty entries.
func splitList(raw string) []string {
	var out []string
	for v := range strings.SplitSeq(raw, ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// SetVersionInfo sets the version string shown by --version.
func SetVersionInfo(version, commit, date string) {
	buildVersion = version
	rootCmd.Version = version + " (" + commit + " " + date + ")"
}

// Execute runs the root command.
func Execute() {
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func run(_ *cobra.Command, _ []string) error {
	log, err := logging.New(os.Stderr, flagLogLevel, flagLogFormat)
	if err != nil {
		return err
	}
	// The root context scopes runs, connections, and the hub; it ends when this
	// function returns, so their Done branches are reachable, not dead code.
	ctx, stopRoot := context.WithCancel(logging.Into(context.Background(), log))
	defer stopRoot()
	// The maintenance loops (approval reaper, trace retention, MCP auto-connect,
	// wake-up drain) get their own cancellation so shutdown can stop them FIRST:
	// a reaper still ticking during the drain could expire the very approval the
	// drain is persisting.
	bgCtx, stopBg := context.WithCancel(ctx)
	defer stopBg()

	baseURL := ""
	if flagBaseURL != "" {
		var err error
		if baseURL, err = server.NormalizeBaseURL(flagBaseURL); err != nil {
			return err
		}
	}
	// The preview runs on its own origin so an untrusted previewed page cannot
	// read the app's token (decisions §5.37): a second port by default, or a
	// configured origin behind a reverse proxy.
	previewPort := flagPreviewPort
	if previewPort == 0 {
		previewPort = flagPort + 1
	}
	if previewPort == flagPort || previewPort < 1 || previewPort > 65535 {
		return fmt.Errorf("--preview-port must be 1-65535 and differ from --port (%d)", flagPort)
	}
	previewOrigin := handler.PreviewOrigin{Port: previewPort}
	if flagPreviewBaseURL != "" {
		if previewOrigin.BaseURL, err = server.NormalizeBaseURL(flagPreviewBaseURL); err != nil {
			return fmt.Errorf("invalid --preview-base-url: %w", err)
		}
	}
	// Stored credentials are sealed under one process key. Without a key they
	// are plaintext — the single-user workbench — and the log says so once.
	box, err := secrets.FromEnvOrFile("AGENTS_SECRET_KEY", flagSecretKeyFile)
	if err != nil {
		return err
	}
	store.UseSecretBox(box)
	if box == nil {
		log.Warn("stored credentials are not encrypted at rest; set AGENTS_SECRET_KEY (or --secret-key-file) to seal them")
	}

	db, err := store.OpenDB(flagDB)
	if err != nil {
		return fmt.Errorf("opening database: %w", err)
	}
	defer db.Close()
	if err := store.CreateSchema(ctx, db); err != nil {
		return fmt.Errorf("creating schema: %w", err)
	}
	if err := store.VerifySecretKey(ctx, db); err != nil {
		return err
	}

	st := newStores(db)
	// The audit log: who did what. A process-level retention, not a setting —
	// the log of configuration changes must not be shortened through the API
	// it records.
	recordAudit := auditRecorder(st.Audit, log)
	if flagAuditRetention > 0 {
		go bridge.RunAuditRetention(bgCtx, st.Audit, flagAuditRetention)
	}
	svc := newBridge(ctx, bgCtx, db, st, recordAudit)
	defer svc.Close()
	hs := newHandlers(st, svc, recordAudit, baseURL, previewOrigin)

	// The restart reconciliation, in two halves that have opposite ordering
	// needs.
	//
	// Failing what the process interrupted — tasks and workflow executions,
	// which are tasks — is a pure UPDATE and runs FIRST, synchronously, before
	// anything can serve a request: the sweep has no notion of a live run, so it
	// fails every row still recorded as working — and a retry that slipped in
	// ahead of it would have its fresh run declared dead, its parent woken with
	// a failure that did not happen, and the real result discarded when the run
	// finally lands.
	//
	// Draining the wake-ups it owes starts runs, so it stays on its own
	// goroutine and AFTER the handlers: NewWSHandler installs
	// runner.OnRunAttach, an ordinary field with no synchronization, and a run
	// starting here would read it while the main goroutine was still writing.
	svc.Runner.FailOrphanedTasks(ctx)
	go svc.Runner.DrainPendingWakeups(bgCtx)
	// The reaper and the clock start after the sweep AND after the handlers,
	// for the same reason the drain does: they end and start runs, and they
	// announce through hooks (OnBroadcast) the WS handler has only now wired.
	go bridge.RunApprovalReaper(bgCtx, st.SettingReader, st.PendingApprovals, st.Entries, st.Tasks, svc.Runner.AnnounceTask)
	if err := svc.Scheduler.Start(ctx); err != nil {
		return fmt.Errorf("starting the trigger scheduler: %w", err)
	}
	defer svc.Scheduler.Stop()

	authSvc, err := newAuth(ctx, st, baseURL, log)
	if err != nil {
		return err
	}
	go bridge.RunAuthTokenCleanup(bgCtx, st.AuthTokens)
	go bridge.RunWakeupCleanup(bgCtx, st.Wakeups)

	srv, err := newServer(ctx, log, authSvc, recordAudit, st, hs, baseURL)
	if err != nil {
		return err
	}

	addr := fmt.Sprintf("%s:%d", flagHost, flagPort)
	httpSrv := &http.Server{
		Addr:    addr,
		Handler: srv.Engine,
		// Slow-loris protection: headers, the whole request read (a client
		// dribbling a BODY would otherwise hold the connection), and idle
		// keep-alive. ReadTimeout covers the request only — net/http lifts it
		// once the body is consumed, so a long response (SSE) is not cut by it,
		// and the WebSocket endpoints hijack and keep their own deadlines. No
		// WriteTimeout: it would abort those streams; each bounds its own writes.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	go func() {
		log.Info("server started", "addr", addr)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			// Nothing above can recover from a dead listener, and staying up
			// would leave a process serving nobody.
			log.Error("server error", "error", err)
			os.Exit(1)
		}
	}()

	// The preview listener: a second origin serving only sandbox previews, so a
	// previewed page never shares the app origin's stored token (decisions
	// §5.37). Its failure is not fatal — the app runs without previews — but the
	// grant URLs would point at a dead port, so the log says so loudly.
	previewEngine, err := server.NewPreviewEngine(log, hs.API.Projects.Preview, splitList(flagTrustedProxies))
	if err != nil {
		return fmt.Errorf("building the preview engine: %w", err)
	}
	previewAddr := fmt.Sprintf("%s:%d", flagHost, previewPort)
	previewSrv := &http.Server{
		Addr:              previewAddr,
		Handler:           previewEngine,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	go func() {
		log.Info("preview listener started", "addr", previewAddr)
		if err := previewSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("preview listener stopped; port previews will not open until it is fixed", "error", err, "addr", previewAddr)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Info("shutting down")
	// The clock first: a tick during the drain would only start a run the
	// drain refuses, recorded on the trigger as a failure that was nobody's.
	svc.Scheduler.Stop()
	// Then the maintenance loops, for the reason at bgCtx's creation.
	stopBg()
	// Drain FIRST, then the listener. Each live run is cancelled and waited
	// for, so its partial turn persists (run.cancelled, savePartialTurn)
	// instead of vanishing when the process exits under it — and ending the
	// runs is also what lets the long-lived SSE handlers return, so
	// httpSrv.Shutdown does not spend its whole budget waiting on event
	// streams that were waiting on those very runs.
	drainCtx, cancelDrain := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelDrain()
	svc.Runner.Shutdown(drainCtx)

	// The WebSocket clients hear a going-away frame rather than a dropped
	// TCP connection (hijacked connections are outside Shutdown's reach).
	srv.Conns.CloseAll("server shutting down")
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = previewSrv.Shutdown(shutCtx)
	if err := httpSrv.Shutdown(shutCtx); err != nil {
		// The runs are drained and persisted by now; whatever kept Shutdown
		// waiting is not worth an exit status.
		log.Warn("http shutdown did not complete cleanly", "error", err)
	}
	return nil
}
