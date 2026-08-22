// Package cmd implements the agents-server command-line entry point and server bootstrap.
package cmd

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/zzir/agents-go/cmd/agents-server/internal/authn"
	"github.com/zzir/agents-go/cmd/agents-server/internal/bridge"
	"github.com/zzir/agents-go/cmd/agents-server/internal/docs"
	"github.com/zzir/agents-go/cmd/agents-server/internal/handler"
	"github.com/zzir/agents-go/cmd/agents-server/internal/logging"
	"github.com/zzir/agents-go/cmd/agents-server/internal/protocol"
	"github.com/zzir/agents-go/cmd/agents-server/internal/secrets"
	"github.com/zzir/agents-go/cmd/agents-server/internal/server"
	"github.com/zzir/agents-go/cmd/agents-server/internal/settings"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
	"github.com/zzir/agents-go/cmd/agents-server/internal/web"
)

var (
	flagHost              string
	flagPort              int
	flagDB                string
	flagWorkspace         string
	flagToken             string
	flagAllowLocalSandbox bool
	flagMaxTasks          int
	flagLogLevel          string
	flagLogFormat         string
	flagBaseURL           string
	flagTrustedProxies    string
	flagAuthMode          string
	flagGoogleClientID    string
	flagGoogleSecret      string
	flagAllowedDomains    string
	flagAllowedEmails     string
	flagBootstrapAdmin    string
	flagAuditRetention    int
	flagSecretKeyFile     string
)

var rootCmd = &cobra.Command{
	Use:   "agents-go server",
	Short: "A web server for the agents-go SDK",
	RunE:  run,
}

func init() {
	rootCmd.Flags().StringVar(&flagHost, "host", "127.0.0.1", "Bind address (use 0.0.0.0 for LAN access)")
	rootCmd.Flags().IntVar(&flagPort, "port", 9527, "HTTP server port")
	rootCmd.Flags().StringVar(&flagDB, "db", "data.db", "SQLite database path, or a postgres:// DSN")
	rootCmd.Flags().StringVar(&flagWorkspace, "workspace", ".", "Workspace directory")
	rootCmd.Flags().StringVar(&flagToken, "token", "", "Authentication token (auto-generated if empty)")
	rootCmd.Flags().BoolVar(&flagAllowLocalSandbox, "allow-local-sandbox", false, "Allow creating local (non-isolated) sandboxes")
	rootCmd.Flags().IntVar(&flagMaxTasks, "max-tasks", 0, "Max live background tasks per session (0 = default 6)")
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
	for _, v := range strings.Split(raw, ",") {
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

	sessionStore := store.NewSessionStore(db)
	entryStore := store.NewSharedEntryStore(db)
	traceStore := store.NewTraceStore(db)
	agentConfigStore := store.NewAgentConfigStore(db)
	mcpServerStore := store.NewMcpServerStore(db)
	memoryStore := store.NewMemoryStore(db)
	settingStore := store.NewSettingStore(db)
	settingStore.SealIf(settings.IsSecret)
	settingReader := settings.NewReader(settingStore)
	providerRouteStore := store.NewProviderRouteStore(db)
	sandboxStore := store.NewSandboxStore(db)
	guardrailStore := store.NewGuardrailStore(db)
	pendingApprovalStore := store.NewPendingApprovalStore(db)
	taskStore := store.NewTaskStore(db)
	providerStore := store.NewProviderStore(db)
	workflowStore := store.NewWorkflowStore(db)
	triggerStore := store.NewTriggerStore(db)
	wakeupStore := store.NewWakeupStore(db)
	userStore := store.NewUserStore(db)
	contextProfileStore := store.NewContextProfileStore(db)
	guardrailResolver := bridge.NewGuardrailResolver(guardrailStore)
	mcpManager := bridge.NewMcpManager(ctx, settingReader)
	oauthCoordinator := bridge.NewOAuthCoordinator(mcpServerStore)
	chatgptOAuth := bridge.NewChatGPTOAuth(providerStore, settingReader)
	defer mcpManager.CloseAll()
	go bridge.ConnectEnabledMcpServers(bgCtx, mcpManager, mcpServerStore, oauthCoordinator)
	go bridge.RunTraceRetention(bgCtx, settingReader, traceStore)
	sandboxManager := bridge.NewSandboxManager(flagWorkspace)
	defer sandboxManager.CloseAll()

	deps := &bridge.AgentDeps{
		AgentConfigs:     agentConfigStore,
		Providers:        providerStore,
		McpServers:       mcpServerStore,
		SandboxConfigs:   sandboxStore,
		Memories:         memoryStore,
		Settings:         settingReader,
		ProviderRoutes:   providerRouteStore,
		Sessions:         sessionStore,
		Traces:           traceStore,
		Guardrails:       guardrailResolver,
		McpManager:       mcpManager,
		SandboxManager:   sandboxManager,
		ChatGPTOAuth:     chatgptOAuth,
		PendingApprovals: pendingApprovalStore,
		Tasks:            taskStore,
		ContextProfiles:  contextProfileStore,
		Workflows:        workflowStore,
		Users:            userStore,
		Wakeups:          wakeupStore,
		Workspace:        flagWorkspace,
		MaxTasks:         flagMaxTasks,
	}
	runner := bridge.NewRunner(ctx, db, deps)

	sessionHandler := handler.NewSessionHandler(handler.SessionDeps{
		Sessions: sessionStore, Entries: entryStore, Traces: traceStore, Agents: agentConfigStore,
		Profiles: contextProfileStore, MCP: mcpManager, MCPServers: mcpServerStore, Users: userStore,
		Stopper: runner, Compactor: runner,
	})
	agentConfigHandler := handler.NewAgentConfigHandler(agentConfigStore, mcpServerStore, providerStore, guardrailResolver)
	mcpServerHandler := handler.NewMcpServerHandler(mcpServerStore, mcpManager, oauthCoordinator, baseURL)
	memoryHandler := handler.NewMemoryHandler(memoryStore)
	settingHandler := handler.NewSettingHandler(settingStore)
	skillHandler := handler.NewSkillHandler(flagWorkspace)
	providerHandler := handler.NewProviderHandler(providerStore)
	workflowHandler := handler.NewWorkflowHandler(workflowStore, agentConfigStore, sessionStore, runner)
	triggerScheduler := bridge.NewTriggerScheduler(runner, triggerStore)
	triggerHandler := handler.NewTriggerHandler(triggerStore, sessionStore, triggerScheduler)
	providerRouteHandler := handler.NewProviderRouteHandler(providerRouteStore, providerStore)
	guardrailHandler := handler.NewGuardrailHandler(guardrailStore, guardrailResolver)
	terminalHandler := handler.NewTerminalHandler(sandboxStore, sandboxManager, settingReader)
	sandboxHandler := handler.NewSandboxHandler(sandboxStore, sandboxManager, flagAllowLocalSandbox, terminalHandler, flagWorkspace)
	traceHandler := handler.NewTraceHandler(traceStore)
	playgroundHandler := handler.NewPlaygroundHandler(deps)
	chatgptOAuthHandler := handler.NewChatGPTOAuthHandler(chatgptOAuth)
	runHandler := handler.NewRunHandler(runner)
	approvalHandler := handler.NewApprovalHandler(pendingApprovalStore, runner)
	taskHandler := handler.NewTaskHandler(taskStore, runner)
	wsHandler := handler.NewWSHandler(runner, sessionStore, pendingApprovalStore)

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
	runner.FailOrphanedTasks(ctx)
	go runner.DrainPendingWakeups(bgCtx)
	// The reaper and the clock start after the sweep AND after the handlers,
	// for the same reason the drain does: they end and start runs, and they
	// announce through hooks (OnBroadcast) the WS handler has only now wired.
	go bridge.RunApprovalReaper(bgCtx, settingReader, pendingApprovalStore, entryStore, taskStore, runner.AnnounceTask)
	if err := triggerScheduler.Start(ctx); err != nil {
		return fmt.Errorf("starting the trigger scheduler: %w", err)
	}
	defer triggerScheduler.Stop()

	// Absolute, because "." means nothing to a browser reading it.
	workspaceAbs, err := filepath.Abs(flagWorkspace)
	if err != nil {
		return fmt.Errorf("resolving the workspace path: %w", err)
	}

	// Both modes keep the implicit local account, so ownership always has a
	// referent — token mode authenticates as it, OAuth mode leaves it dormant
	// (no identity, no token, no way to sign in as it).
	localUser, err := userStore.EnsureLocalUser(ctx)
	if err != nil {
		return fmt.Errorf("ensuring the local user: %w", err)
	}
	authTokens := store.NewAuthTokenStore(db)

	var authSvc *authn.Service
	switch flagAuthMode {
	case "token":
		token := flagToken
		if token == "" {
			token = server.GenerateToken()
		}
		log.Info("auth token", "token", token)
		authSvc = authn.NewStatic(token, localUser)
	case "oauth":
		// Fail-fast on a combination that could not be signed in to, or that
		// would admit everyone: OAuth mode demands an explicit allowlist.
		if flagToken != "" {
			return fmt.Errorf("--token cannot combine with --auth oauth; programmatic access uses personal access tokens")
		}
		if baseURL == "" {
			return fmt.Errorf("--auth oauth requires --base-url: the OAuth redirect URI derives from it")
		}
		googleSecret := flagGoogleSecret
		if googleSecret == "" {
			googleSecret = os.Getenv("AGENTS_OAUTH_GOOGLE_CLIENT_SECRET")
		}
		var providers []authn.OAuthProvider
		if flagGoogleClientID != "" {
			if googleSecret == "" {
				return fmt.Errorf("google login needs --oauth-google-client-secret (or AGENTS_OAUTH_GOOGLE_CLIENT_SECRET)")
			}
			providers = append(providers, &authn.Google{ClientID: flagGoogleClientID, ClientSecret: googleSecret})
		}
		if len(providers) == 0 {
			return fmt.Errorf("--auth oauth needs at least one provider (--oauth-google-client-id)")
		}
		domains, emails := splitList(flagAllowedDomains), splitList(flagAllowedEmails)
		bootstrapAdmin := strings.ToLower(strings.TrimSpace(flagBootstrapAdmin))
		for _, d := range domains {
			if strings.Contains(d, "@") {
				return fmt.Errorf("--allowed-domains takes domains, not addresses: %q", d)
			}
		}
		for _, e := range append(emails, bootstrapAdmin) {
			if e != "" && !strings.Contains(e, "@") {
				return fmt.Errorf("--allowed-emails and --bootstrap-admin take addresses: %q", e)
			}
		}
		if flagBootstrapAdmin != "" && bootstrapAdmin == "" {
			return fmt.Errorf("--bootstrap-admin is blank")
		}
		if len(domains) == 0 && len(emails) == 0 && bootstrapAdmin == "" {
			return fmt.Errorf("--auth oauth needs an allowlist: --allowed-domains, --allowed-emails or --bootstrap-admin")
		}
		if bootstrapAdmin == "" {
			// Without a named admin the first account to sign in is it — a
			// race anyone on an allowed domain can win until someone has.
			if n, err := userStore.CountReal(ctx); err != nil {
				return fmt.Errorf("counting users: %w", err)
			} else if n == 0 {
				log.Warn("no admin account yet: the first OAuth login becomes the admin — name one with --bootstrap-admin to choose who")
			}
		}
		authSvc = authn.NewOAuth(authn.OAuthConfig{
			Users: userStore, Tokens: authTokens,
			BaseURL: baseURL, Providers: providers,
			AllowedDomains: domains, AllowedEmails: emails,
			BootstrapAdmin: bootstrapAdmin, Log: log,
		})
	default:
		return fmt.Errorf("unknown --auth mode %q (token or oauth)", flagAuthMode)
	}
	go bridge.RunAuthTokenCleanup(bgCtx, authTokens)
	go bridge.RunWakeupCleanup(bgCtx, wakeupStore)

	// The audit log: who did what. A process-level retention, not a setting —
	// the log of configuration changes must not be shortened through the API
	// it records.
	auditStore := store.NewAuditStore(db)
	recordAudit := func(ctx context.Context, r protocol.AuditRecord) {
		err := auditStore.Record(ctx, &store.AuditEvent{
			ActorID: r.Actor.ID, ActorEmail: r.Actor.Email,
			Action: r.Action, Resource: r.Resource, Detail: r.Detail, ClientIP: r.ClientIP,
		})
		if err != nil {
			log.Error("audit record failed", "error", err, "action", r.Action)
		}
	}
	if flagAuditRetention > 0 {
		go bridge.RunAuditRetention(bgCtx, auditStore, flagAuditRetention)
	}
	wsHandler.Audit = recordAudit
	terminalHandler.Audit = recordAudit
	deps.Audit = recordAudit

	srv := server.New(log, authSvc.Authenticate, recordAudit)
	srv.SetImageHosts(authSvc.AvatarHosts())
	// A replay posts a stored span payload back: the body cap follows the
	// size the settings let a span keep, plus room for the rest of the request.
	srv.SetBodyLimit(server.APIPrefix+"/playground/generate", func() int64 {
		return int64(settingReader.Int(ctx, settings.KeyTraceSpanDataKB))*1024 + 256*1024
	})
	authHandler := handler.NewAuthHandler(authSvc, authTokens, userStore, auditStore)
	authHandler.Conns = srv.Conns
	if flagTrustedProxies != "" {
		proxies := strings.Split(flagTrustedProxies, ",")
		for i := range proxies {
			proxies[i] = strings.TrimSpace(proxies[i])
		}
		if err := srv.SetTrustedProxies(proxies); err != nil {
			return fmt.Errorf("invalid --trusted-proxies: %w", err)
		}
	} else if baseURL != "" {
		// --base-url is the "behind a proxy" signal; without trusted proxies
		// every client shares the proxy's IP and so one per-IP budget.
		log.Warn("--base-url is set but --trusted-proxies is not: every request arrives from the proxy's address, so all users share one per-IP rate budget")
	}
	srv.RegisterAPI(handler.Handlers{
		Authz: handler.AuthzDeps{
			Sessions: sessionStore, Tasks: taskStore, Approvals: pendingApprovalStore,
			Triggers: triggerStore, Hub: runner.Hub(),
		},
		Auth:           authHandler,
		Sessions:       sessionHandler,
		Runs:           runHandler,
		Approvals:      approvalHandler,
		Tasks:          taskHandler,
		Agents:         agentConfigHandler,
		McpServers:     mcpServerHandler,
		Memories:       memoryHandler,
		Settings:       settingHandler,
		Skills:         skillHandler,
		Providers:      providerHandler,
		Workflows:      workflowHandler,
		Triggers:       triggerHandler,
		ProviderRoutes: providerRouteHandler,
		Guardrails:     guardrailHandler,
		Sandboxes:      sandboxHandler,
		Traces:         traceHandler,
		Playground:     playgroundHandler,
		ChatGPT:        chatgptOAuthHandler,
		Server: handler.ServerInfo{
			Version:           buildVersion,
			Workspace:         workspaceAbs,
			AllowLocalSandbox: flagAllowLocalSandbox,
			MaxTasks:          runner.Hub().MaxTasks(),
		},
	}.Register)
	srv.RegisterWS(wsHandler.Handle, terminalHandler.Handle)
	srv.RegisterHook(triggerHandler.Hook)

	srv.ServeHealth(buildVersion)
	srv.ServeOpenAPI(docs.SpecYAML)

	staticFS, err := fs.Sub(web.StaticFS, "frontend/dist")
	if err != nil {
		return fmt.Errorf("embedding static files: %w", err)
	}
	srv.ServeStatic(staticFS)

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
		log.Info("server started", "addr", addr, "workspace", flagWorkspace)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			// Nothing above can recover from a dead listener, and staying up
			// would leave a process serving nobody.
			log.Error("server error", "error", err)
			os.Exit(1)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Info("shutting down")
	// The clock first: a tick during the drain would only start a run the
	// drain refuses, recorded on the trigger as a failure that was nobody's.
	triggerScheduler.Stop()
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
	runner.Shutdown(drainCtx)

	// The WebSocket clients hear a going-away frame rather than a dropped
	// TCP connection (hijacked connections are outside Shutdown's reach).
	srv.Conns.CloseAll("server shutting down")
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutCtx); err != nil {
		// The runs are drained and persisted by now; whatever kept Shutdown
		// waiting is not worth an exit status.
		log.Warn("http shutdown did not complete cleanly", "error", err)
	}
	return nil
}
