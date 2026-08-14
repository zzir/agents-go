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
	"syscall"
	"time"

	"github.com/rs/zerolog"
	"github.com/spf13/cobra"

	"github.com/zzir/agents-go/cmd/agents-server/internal/bridge"
	"github.com/zzir/agents-go/cmd/agents-server/internal/docs"
	"github.com/zzir/agents-go/cmd/agents-server/internal/handler"
	"github.com/zzir/agents-go/cmd/agents-server/internal/server"
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
)

var rootCmd = &cobra.Command{
	Use:   "agents-go server",
	Short: "A web server for the agents-go SDK",
	RunE:  run,
}

func init() {
	rootCmd.Flags().StringVar(&flagHost, "host", "127.0.0.1", "Bind address (use 0.0.0.0 for LAN access)")
	rootCmd.Flags().IntVar(&flagPort, "port", 9527, "HTTP server port")
	rootCmd.Flags().StringVar(&flagDB, "db", "data.db", "SQLite database path")
	rootCmd.Flags().StringVar(&flagWorkspace, "workspace", ".", "Workspace directory")
	rootCmd.Flags().StringVar(&flagToken, "token", "", "Authentication token (auto-generated if empty)")
	rootCmd.Flags().BoolVar(&flagAllowLocalSandbox, "allow-local-sandbox", false, "Allow creating local (non-isolated) sandboxes")
	rootCmd.Flags().IntVar(&flagMaxTasks, "max-tasks", 0, "Max live background tasks per session (0 = default 6)")
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

func run(_ *cobra.Command, _ []string) error {
	log := zerolog.New(zerolog.ConsoleWriter{Out: os.Stderr, TimeFormat: time.RFC3339}).
		With().Timestamp().Logger()

	ctx := log.WithContext(context.Background())

	dsn := fmt.Sprintf("file:%s?cache=shared&_journal_mode=WAL", flagDB)
	db, err := store.NewSQLiteDB(dsn)
	if err != nil {
		return fmt.Errorf("opening database: %w", err)
	}
	defer db.Close()

	if err := store.CreateSchema(ctx, db); err != nil {
		return fmt.Errorf("creating schema: %w", err)
	}

	sessionStore := store.NewSessionStore(db)
	entryStore := store.NewSharedEntryStore(db)
	traceStore := store.NewTraceStore(db)
	agentConfigStore := store.NewAgentConfigStore(db)
	mcpServerStore := store.NewMcpServerStore(db)
	memoryStore := store.NewMemoryStore(db)
	settingStore := store.NewSettingStore(db)
	providerRouteStore := store.NewProviderRouteStore(db)
	sandboxStore := store.NewSandboxStore(db)
	guardrailStore := store.NewGuardrailStore(db)
	pendingApprovalStore := store.NewPendingApprovalStore(db)
	taskStore := store.NewTaskStore(db)
	providerStore := store.NewProviderStore(db)
	workflowStore := store.NewWorkflowStore(db)
	workflowRunStore := store.NewWorkflowRunStore(db)
	wakeupStore := store.NewWakeupStore(db)
	contextProfileStore := store.NewContextProfileStore(db)
	guardrailResolver := bridge.NewGuardrailResolver(guardrailStore)
	mcpManager := bridge.NewMcpManager(ctx, settingStore)
	oauthCoordinator := bridge.NewOAuthCoordinator(mcpServerStore)
	chatgptOAuth := bridge.NewChatGPTOAuth(providerStore)
	chatgptOAuth.UseSettings(settingStore) // route token refresh/exchange through configured proxy
	defer mcpManager.CloseAll()
	go bridge.ConnectEnabledMcpServers(ctx, mcpManager, mcpServerStore, oauthCoordinator)
	go bridge.RunTraceRetention(ctx, settingStore, traceStore)
	sandboxManager := bridge.NewSandboxManager(flagWorkspace)
	defer sandboxManager.CloseAll()

	deps := &bridge.AgentDeps{
		AgentConfigs:     agentConfigStore,
		Providers:        providerStore,
		McpServers:       mcpServerStore,
		SandboxConfigs:   sandboxStore,
		Memories:         memoryStore,
		Settings:         settingStore,
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
		WorkflowRuns:     workflowRunStore,
		Wakeups:          wakeupStore,
		Workspace:        flagWorkspace,
		MaxTasks:         flagMaxTasks,
	}
	runner := bridge.NewRunner(ctx, db, deps)

	// After the runner: expiring a workflow step's approval has to end that
	// execution too, and only the runner can claim the ending.
	go bridge.RunApprovalReaper(ctx, settingStore, pendingApprovalStore, entryStore, taskStore, wakeupStore, runner.FailWorkflowForExpiredApproval)

	sessionHandler := handler.NewSessionHandler(sessionStore, entryStore, traceStore, agentConfigStore).
		WithRunStopper(runner).WithContextProfiles(contextProfileStore, mcpManager, mcpServerStore).WithCompactor(runner)
	agentConfigHandler := handler.NewAgentConfigHandler(agentConfigStore).WithMcpStore(mcpServerStore).WithGuardrails(guardrailResolver).WithProviders(providerStore)
	mcpServerHandler := handler.NewMcpServerHandler(mcpServerStore, mcpManager, oauthCoordinator)
	memoryHandler := handler.NewMemoryHandler(memoryStore)
	settingHandler := handler.NewSettingHandler(settingStore)
	skillHandler := handler.NewSkillHandler(flagWorkspace)
	providerHandler := handler.NewProviderHandler(providerStore)
	workflowHandler := handler.NewWorkflowHandler(workflowStore, workflowRunStore, agentConfigStore, runner)
	providerRouteHandler := handler.NewProviderRouteHandler(providerRouteStore, providerStore)
	guardrailHandler := handler.NewGuardrailHandler(guardrailStore, guardrailResolver)
	terminalHandler := handler.NewTerminalHandler(sandboxStore, sandboxManager)
	sandboxHandler := handler.NewSandboxHandler(sandboxStore, sandboxManager, flagAllowLocalSandbox).
		WithTerminals(terminalHandler).WithWorkspace(flagWorkspace)
	traceHandler := handler.NewTraceHandler(traceStore)
	playgroundHandler := handler.NewPlaygroundHandler(deps)
	chatgptOAuthHandler := handler.NewChatGPTOAuthHandler(chatgptOAuth)
	runHandler := handler.NewRunHandler(runner)
	approvalHandler := handler.NewApprovalHandler(pendingApprovalStore, runner)
	taskHandler := handler.NewTaskHandler(taskStore, runner)
	wsHandler := handler.NewWSHandler(runner)

	// The restart reconciliation, in two halves that have opposite ordering
	// needs.
	//
	// Failing what the process interrupted is a pure UPDATE and runs FIRST,
	// synchronously, before anything can serve a request: the sweep has no
	// notion of a live run, so it fails every row still recorded as working —
	// and a retry that slipped in ahead of it would have its fresh run declared
	// dead, its parent woken with a failure that did not happen, and the real
	// result discarded when the run finally lands.
	//
	// Draining the wake-ups it owes starts runs, so it stays on its own
	// goroutine and AFTER the handlers: NewWSHandler installs
	// runner.OnRunAttach, an ordinary field with no synchronization, and a run
	// starting here would read it while the main goroutine was still writing.
	runner.FailOrphanedTasks(ctx)
	runner.FailInterruptedWorkflows(ctx)
	go runner.DrainPendingWakeups(ctx)

	token := flagToken
	if token == "" {
		token = server.GenerateToken()
	}
	log.Info().Str("token", token).Msg("auth token")

	srv := server.New(log, token)
	srv.RegisterAPI(handler.Handlers{
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
		ProviderRoutes: providerRouteHandler,
		Guardrails:     guardrailHandler,
		Sandboxes:      sandboxHandler,
		Traces:         traceHandler,
		Playground:     playgroundHandler,
		ChatGPT:        chatgptOAuthHandler,
	}.Register)
	srv.RegisterWS(wsHandler.Handle, terminalHandler.Handle)

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
		// Slow-loris protection: bound how long a client may take to send request
		// headers, and how long an idle keep-alive connection may linger. There is
		// deliberately NO global WriteTimeout — it would abort the long-lived SSE
		// (/runs/{id}/events) and WebSocket (/ws, /ws/terminal) responses mid-stream;
		// those transports bound their own writes (SSE heartbeat, ws write deadline).
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	go func() {
		log.Info().Str("addr", addr).Str("workspace", flagWorkspace).Msg("server started")
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal().Err(err).Msg("server error")
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Info().Msg("shutting down")
	// Drain FIRST, then the listener. Each live run is cancelled and waited
	// for, so its partial turn persists (run.cancelled, savePartialTurn)
	// instead of vanishing when the process exits under it — and ending the
	// runs is also what lets the long-lived SSE handlers return, so
	// httpSrv.Shutdown does not spend its whole budget waiting on event
	// streams that were waiting on those very runs.
	drainCtx, cancelDrain := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelDrain()
	runner.Shutdown(drainCtx)

	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutCtx); err != nil {
		// A hijacked WebSocket keeps Shutdown waiting until its deadline; the
		// runs are already drained and persisted by then, so reporting that as
		// the process's exit status turned every ordinary stop into a failure.
		log.Warn().Err(err).Msg("http shutdown did not complete cleanly")
	}
	return nil
}
