// Package cmd implements the agents-server command-line entry point and server
// bootstrap.
package cmd

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/zzir/agents-go/cmd/agents-server/internal/bridge"
	"github.com/zzir/agents-go/cmd/agents-server/internal/handler"
	"github.com/zzir/agents-go/cmd/agents-server/internal/logging"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// rootCmd is the command main executes; a test builds its own (newRootCmd).
var rootCmd = newRootCmd()

// newRootCmd defines the command and its flags. Nothing is bound to a
// variable: loadConfig reads the parsed flags into the config run passes on.
func newRootCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "agents-server",
		Short: "The Go-native agent workbench you run yourself",
		RunE:  run,
	}
	f := cmd.Flags()
	f.String("host", "127.0.0.1", "Bind address (use 0.0.0.0 for LAN access)")
	f.Int("port", 9527, "HTTP server port")
	f.String("db", "data.db", "SQLite database path, or a postgres:// DSN (or env AGENTS_DB)")
	f.String("token", "", "Authentication token (or env AGENTS_TOKEN; auto-generated if empty)")
	f.String("log-level", "info", "Log level: debug, info, warn, error")
	f.String("log-format", "text", "Log format: text, json")
	f.String("base-url", "", "Public origin of this server, scheme://host[:port] (required behind a reverse proxy for OAuth flows)")
	f.String("trusted-proxies", "", "Comma-separated proxy IPs/CIDRs whose X-Forwarded-For is believed for client IPs (default: none)")
	f.String("auth", "token", "Authentication mode: token (single static token) or oauth (per-user login)")
	f.String("oauth-google-client-id", "", "Google OAuth client id (enables the google login provider)")
	f.String("oauth-google-client-secret", "", "Google OAuth client secret (or env AGENTS_OAUTH_GOOGLE_CLIENT_SECRET)")
	f.String("oauth-github-client-id", "", "GitHub OAuth client id (enables the github login provider)")
	f.String("oauth-github-client-secret", "", "GitHub OAuth client secret (or env AGENTS_OAUTH_GITHUB_CLIENT_SECRET)")
	f.String("allowed-domains", "", "Comma-separated email domains admitted to OAuth login")
	f.String("allowed-emails", "", "Comma-separated email addresses admitted to OAuth login")
	f.String("bootstrap-admin", "", "Email that signs in as admin (implicitly admitted; the recovery hatch)")
	f.Int("audit-retention-days", 0, "Prune audit log entries older than this many days (0 = keep forever); a process setting, never an API one")
	f.String("secret-key-file", "", "File holding the 32-byte key (base64 or hex) that seals stored credentials; or env AGENTS_SECRET_KEY")
	return cmd
}

// buildVersion is the plain version string (without commit/date), surfaced by
// the /health endpoint.
var buildVersion = "dev"

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

// run is the command's RunE: the flags become a config, and serve runs on it.
func run(cmd *cobra.Command, _ []string) error {
	cfg, err := loadConfig(cmd)
	if err != nil {
		return err
	}
	return serve(cfg)
}

// serve runs the server on cfg until a signal or a dead listener ends it.
func serve(cfg *config) error {
	log, err := logging.New(os.Stderr, cfg.LogLevel, cfg.LogFormat)
	if err != nil {
		return err
	}
	// The root context scopes runs, connections and the hub; it ends when serve returns.
	ctx, stopRoot := context.WithCancel(logging.Into(context.Background(), log))
	defer stopRoot()
	// The maintenance loops get their own cancellation so shutdown stops them
	// first — invariant 43.
	bgCtx, stopBg := context.WithCancel(ctx)
	defer stopBg()

	// Bound before the database is opened: a second start on a taken address
	// is refused before it can sweep a live instance's tasks — invariant 63.
	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", addr, err)
	}
	defer ln.Close()

	// Stored credentials are sealed under one process key. Without a key they
	// are plaintext — the single-user workbench — and the log says so once.
	store.UseSecretBox(cfg.SecretBox)
	if cfg.SecretBox == nil {
		log.Warn("stored credentials are not encrypted at rest; set AGENTS_SECRET_KEY (or --secret-key-file) to seal them")
	}

	db, err := store.OpenDB(cfg.DB)
	if err != nil {
		return fmt.Errorf("opening database: %w", err)
	}
	defer db.Close()
	// One instance per database, before the orphan sweep — workbench invariant 63.
	releaseInstanceLock, err := store.AcquireInstanceLock(ctx, db)
	if err != nil {
		return err
	}
	defer releaseInstanceLock()
	if err := store.CreateSchema(ctx, db); err != nil {
		return fmt.Errorf("creating schema: %w", err)
	}
	if err := store.VerifySecretKey(ctx, db); err != nil {
		return err
	}

	st := newStores(db)
	// The audit log; its retention is a flag, not a setting (invariant 54): the
	// log of configuration changes is not shortened through the API it records.
	recordAudit := auditRecorder(st.Audit, log)
	if cfg.AuditRetentionDays > 0 {
		go bridge.RunAuditRetention(bgCtx, st.Audit, cfg.AuditRetentionDays)
	}
	svc, err := newBridge(ctx, bgCtx, db, st, recordAudit)
	if err != nil {
		return err
	}
	defer svc.Close()
	hs := newHandlers(st, svc, recordAudit, cfg.BaseURL, handler.ServerInfo{
		Version:           buildVersion,
		Timezone:          serverTimezone(),
		CredentialsSealed: cfg.SecretBox != nil,
	})

	// What is left of flag validation (--trusted-proxies) happens before the
	// sweep below changes a row.
	authSvc, err := newAuth(ctx, st, cfg, log)
	if err != nil {
		return err
	}
	srv, err := newServer(ctx, log, authSvc, recordAudit, st, hs, cfg)
	if err != nil {
		return err
	}

	// Restart reconciliation: fail the interrupted first, drain the wake-ups
	// after the handlers — invariant 32.
	svc.Runner.FailOrphanedTasks(ctx)
	go svc.Runner.DrainPendingWakeups(bgCtx)
	// The reaper and the clock start after the sweep and the handlers: they
	// announce through hooks the WS handler has only now wired.
	go svc.Runner.RunApprovalReaper(bgCtx)
	if err := svc.Scheduler.Start(ctx); err != nil {
		return fmt.Errorf("starting the trigger scheduler: %w", err)
	}
	defer svc.Scheduler.Stop()

	go bridge.RunAuthTokenCleanup(bgCtx, st.AuthTokens)
	go bridge.RunWakeupCleanup(bgCtx, st.Wakeups)
	go bridge.RunAttachmentReaper(bgCtx, st.SettingReader, st.Attachments)
	go bridge.RunTaskSessionRetention(bgCtx, st.SettingReader, st.Sessions)

	httpSrv := &http.Server{
		Handler: srv.Engine,
		// Slow-loris protection on headers, body and idle keep-alive; no
		// WriteTimeout, as the streams (SSE, WebSocket) bound their own writes.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	serveErr := make(chan error, 1)
	go func() {
		log.Info("server started", "addr", addr)
		if err := httpSrv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
	}()

	sigCtx, stopSignals := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stopSignals()
	var serveFailed error
	select {
	case <-sigCtx.Done():
	case serveFailed = <-serveErr:
		// A dead listener leaves a process serving nobody: shut down in order.
		log.Error("server error", "error", serveFailed)
	}

	log.Info("shutting down")
	// Shutdown order: the clock, the maintenance loops, the drain, then the
	// listener — invariant 43.
	svc.Scheduler.Stop()
	stopBg()
	drainCtx, cancelDrain := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelDrain()
	svc.Runner.Shutdown(drainCtx)

	// WebSocket clients hear a going-away frame (hijacked connections are
	// outside Shutdown's reach).
	srv.Conns.CloseAll("server shutting down")
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutCtx); err != nil {
		// The runs are drained by now; a late Shutdown is not worth an exit status.
		log.Warn("http shutdown did not complete cleanly", "error", err)
	}
	// The root context ends before the deferred closes tear down what descends from it.
	stopRoot()
	return serveFailed
}

// serverTimezone names the zone cron schedules run in: the IANA name when TZ
// is set, else the abbreviation and offset so "Local" still says which clock.
func serverTimezone() string {
	name := time.Local.String()
	if name != "Local" {
		return name
	}
	return formatLocalZone(time.Now().Zone())
}

// formatLocalZone renders an unnamed zone as "Local (CST UTC+08:00)"; an
// abbreviation that is itself an offset ("+08") would repeat it, so it is dropped.
func formatLocalZone(abbr string, off int) string {
	sign := "+"
	if off < 0 {
		sign, off = "-", -off
	}
	if abbr != "" && (abbr[0] == '+' || abbr[0] == '-') {
		abbr = ""
	}
	if abbr != "" {
		abbr += " "
	}
	return fmt.Sprintf("Local (%sUTC%s%02d:%02d)", abbr, sign, off/3600, off%3600/60)
}
