// Package cmd implements the agents-server command-line entry point and server bootstrap.
package cmd

import (
	"context"
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
	flagPort              int
	flagDB                string
	flagWorkspace         string
	flagToken             string
	flagAllowLocalSandbox bool
)

var rootCmd = &cobra.Command{
	Use:   "agents-server",
	Short: "A web server for the agents-go SDK",
	RunE:  run,
}

func init() {
	rootCmd.Flags().IntVar(&flagPort, "port", 9527, "HTTP server port")
	rootCmd.Flags().StringVar(&flagDB, "db", "data.db", "SQLite database path")
	rootCmd.Flags().StringVar(&flagWorkspace, "workspace", ".", "Workspace directory")
	rootCmd.Flags().StringVar(&flagToken, "token", "", "Authentication token (auto-generated if empty)")
	rootCmd.Flags().BoolVar(&flagAllowLocalSandbox, "allow-local-sandbox", false, "Allow creating local (non-isolated) sandboxes")
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
	messageStore := store.NewMessageStore(db)
	traceStore := store.NewTraceStore(db)
	agentConfigStore := store.NewAgentConfigStore(db)
	mcpServerStore := store.NewMcpServerStore(db)
	memoryStore := store.NewMemoryStore(db)
	settingStore := store.NewSettingStore(db)
	providerRouteStore := store.NewProviderRouteStore(db)
	sandboxStore := store.NewSandboxStore(db)
	guardrailStore := store.NewGuardrailStore(db)
	pendingApprovalStore := store.NewPendingApprovalStore(db)
	guardrailResolver := bridge.NewGuardrailResolver(guardrailStore)
	mcpManager := bridge.NewMcpManager(ctx, settingStore)
	oauthCoordinator := bridge.NewOAuthCoordinator(mcpServerStore)
	chatgptOAuth := bridge.NewChatGPTOAuth(agentConfigStore)
	defer mcpManager.CloseAll()
	go bridge.ConnectEnabledMcpServers(ctx, mcpManager, mcpServerStore, oauthCoordinator)
	go bridge.RunTraceRetention(ctx, settingStore, traceStore)
	go bridge.RunApprovalReaper(ctx, settingStore, pendingApprovalStore, messageStore)
	sandboxManager := bridge.NewSandboxManager(flagWorkspace)
	defer sandboxManager.CloseAll()

	deps := &bridge.AgentDeps{
		AgentConfigs:     agentConfigStore,
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
		Workspace:        flagWorkspace,
	}
	runner := bridge.NewRunner(ctx, db, deps)

	sessionHandler := handler.NewSessionHandler(sessionStore, messageStore, traceStore, agentConfigStore)
	agentConfigHandler := handler.NewAgentConfigHandler(agentConfigStore).WithMcpStore(mcpServerStore).WithGuardrails(guardrailResolver)
	mcpServerHandler := handler.NewMcpServerHandler(mcpServerStore, mcpManager, oauthCoordinator)
	memoryHandler := handler.NewMemoryHandler(memoryStore)
	settingHandler := handler.NewSettingHandler(settingStore)
	skillHandler := handler.NewSkillHandler(flagWorkspace)
	providerRouteHandler := handler.NewProviderRouteHandler(providerRouteStore)
	guardrailHandler := handler.NewGuardrailHandler(guardrailStore, guardrailResolver)
	sandboxHandler := handler.NewSandboxHandler(sandboxStore, sandboxManager, flagAllowLocalSandbox)
	traceHandler := handler.NewTraceHandler(traceStore)
	playgroundHandler := handler.NewPlaygroundHandler(deps)
	chatgptOAuthHandler := handler.NewChatGPTOAuthHandler(chatgptOAuth)
	runHandler := handler.NewRunHandler(runner)
	approvalHandler := handler.NewApprovalHandler(pendingApprovalStore, runner)
	wsHandler := handler.NewWSHandler(runner)

	token := flagToken
	if token == "" {
		token = server.GenerateToken()
	}
	log.Info().Str("token", token).Msg("auth token")

	srv := server.New(log, token)
	srv.RegisterRoutes(server.Routes{
		SessionList:     sessionHandler.List,
		SessionCreate:   sessionHandler.Create,
		SessionGet:      sessionHandler.Get,
		SessionPatch:    sessionHandler.Patch,
		SessionDelete:   sessionHandler.Delete,
		SessionMessages: sessionHandler.Messages,
		SessionFork:     sessionHandler.Fork,
		WSHandler:       wsHandler.Handle,

		RunCreate: runHandler.Create,
		RunGet:    runHandler.Get,
		RunCancel: runHandler.Cancel,
		RunEvents: runHandler.Events,

		ApprovalListBySession: approvalHandler.ListBySession,
		ApprovalApprove:       approvalHandler.Approve,
		ApprovalReject:        approvalHandler.Reject,

		AgentList:   agentConfigHandler.List,
		AgentCreate: agentConfigHandler.Create,
		AgentGet:    agentConfigHandler.Get,
		AgentUpdate: agentConfigHandler.Update,
		AgentDelete: agentConfigHandler.Delete,

		McpServerList:          mcpServerHandler.List,
		McpServerCreate:        mcpServerHandler.Create,
		McpServerGet:           mcpServerHandler.Get,
		McpServerUpdate:        mcpServerHandler.Update,
		McpServerDelete:        mcpServerHandler.Delete,
		McpServerConnect:       mcpServerHandler.Connect,
		McpServerClearOAuth:    mcpServerHandler.ClearOAuth,
		McpServerTools:         mcpServerHandler.Tools,
		McpServerOAuthCallback: mcpServerHandler.OAuthCallback,

		MemoryList:   memoryHandler.List,
		MemoryCreate: memoryHandler.Create,
		MemoryGet:    memoryHandler.Get,
		MemoryUpdate: memoryHandler.Update,
		MemoryDelete: memoryHandler.Delete,

		SettingList:   settingHandler.List,
		SettingGet:    settingHandler.Get,
		SettingSet:    settingHandler.Set,
		SettingDelete: settingHandler.Delete,

		SkillList:       skillHandler.List,
		SkillGet:        skillHandler.Get,
		SkillRepoClone:  skillHandler.Clone,
		SkillRepoSync:   skillHandler.Sync,
		SkillRepoDelete: skillHandler.Delete,

		ProviderRouteList:   providerRouteHandler.List,
		ProviderRouteCreate: providerRouteHandler.Create,
		ProviderRouteGet:    providerRouteHandler.Get,
		ProviderRouteUpdate: providerRouteHandler.Update,
		ProviderRouteDelete: providerRouteHandler.Delete,

		GuardrailList:   guardrailHandler.List,
		GuardrailCreate: guardrailHandler.Create,
		GuardrailGet:    guardrailHandler.Get,
		GuardrailUpdate: guardrailHandler.Update,
		GuardrailDelete: guardrailHandler.Delete,

		SandboxList:   sandboxHandler.List,
		SandboxCreate: sandboxHandler.Create,
		SandboxGet:    sandboxHandler.Get,
		SandboxUpdate: sandboxHandler.Update,
		SandboxDelete: sandboxHandler.Delete,
		SandboxTest:   sandboxHandler.Test,

		TraceListBySession: traceHandler.ListBySession,

		PlaygroundGenerate: playgroundHandler.Generate,

		ChatGPTLogin:    chatgptOAuthHandler.Login,
		ChatGPTCallback: chatgptOAuthHandler.Callback,
		ChatGPTStatus:   chatgptOAuthHandler.Status,
		ChatGPTLogout:   chatgptOAuthHandler.Logout,
	})

	srv.ServeHealth(buildVersion)
	srv.ServeOpenAPI(docs.SpecYAML)

	staticFS, err := fs.Sub(web.StaticFS, "frontend/dist")
	if err != nil {
		return fmt.Errorf("embedding static files: %w", err)
	}
	srv.ServeStatic(staticFS)

	addr := fmt.Sprintf("127.0.0.1:%d", flagPort)
	httpSrv := &http.Server{Addr: addr, Handler: srv.Engine}

	go func() {
		log.Info().Str("addr", addr).Str("workspace", flagWorkspace).Msg("server started")
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal().Err(err).Msg("server error")
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Info().Msg("shutting down")
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return httpSrv.Shutdown(shutCtx)
}
