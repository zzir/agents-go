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
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/zzir/agents-go/cmd/agents-server/internal/bridge"
	"github.com/zzir/agents-go/cmd/agents-server/internal/docs"
	"github.com/zzir/agents-go/cmd/agents-server/internal/handler"
	"github.com/zzir/agents-go/cmd/agents-server/internal/logging"
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

	db, err := store.OpenDB(flagDB)
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
		Wakeups:          wakeupStore,
		Workspace:        flagWorkspace,
		MaxTasks:         flagMaxTasks,
	}
	runner := bridge.NewRunner(ctx, db, deps)

	sessionHandler := handler.NewSessionHandler(handler.SessionDeps{
		Sessions: sessionStore, Entries: entryStore, Traces: traceStore, Agents: agentConfigStore,
		Profiles: contextProfileStore, MCP: mcpManager, MCPServers: mcpServerStore,
		Stopper: runner, Compactor: runner,
	})
	agentConfigHandler := handler.NewAgentConfigHandler(agentConfigStore, mcpServerStore, providerStore, guardrailResolver)
	mcpServerHandler := handler.NewMcpServerHandler(mcpServerStore, mcpManager, oauthCoordinator)
	memoryHandler := handler.NewMemoryHandler(memoryStore)
	settingHandler := handler.NewSettingHandler(settingStore)
	skillHandler := handler.NewSkillHandler(flagWorkspace)
	providerHandler := handler.NewProviderHandler(providerStore)
	workflowHandler := handler.NewWorkflowHandler(workflowStore, agentConfigStore, runner)
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
	wsHandler := handler.NewWSHandler(runner)

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

	token := flagToken
	if token == "" {
		token = server.GenerateToken()
	}
	log.Info("auth token", "token", token)

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
		// Slow-loris protection: bound the headers, the whole request read (or a
		// client dribbling a request BODY holds the connection forever — headers
		// alone don't cover it), and how long an idle keep-alive connection may
		// linger. The long-lived responses opt out of ReadTimeout themselves: the
		// WebSocket endpoints hijack and manage their own deadlines, and the SSE
		// handler clears the read deadline (see RunHandler.Events). There is
		// deliberately NO global WriteTimeout — it would abort those same streams
		// mid-response; they bound their own writes (SSE heartbeat, ws write
		// deadline).
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

	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutCtx); err != nil {
		// A hijacked WebSocket keeps Shutdown waiting until its deadline; the
		// runs are already drained and persisted by then, so reporting that as
		// the process's exit status turned every ordinary stop into a failure.
		log.Warn("http shutdown did not complete cleanly", "error", err)
	}
	return nil
}
