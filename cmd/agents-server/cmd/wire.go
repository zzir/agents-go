package cmd

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"strings"

	"github.com/uptrace/bun"

	"github.com/zzir/agents-go/cmd/agents-server/internal/authn"
	"github.com/zzir/agents-go/cmd/agents-server/internal/bridge"
	"github.com/zzir/agents-go/cmd/agents-server/internal/docs"
	"github.com/zzir/agents-go/cmd/agents-server/internal/guardrails"
	"github.com/zzir/agents-go/cmd/agents-server/internal/handler"
	"github.com/zzir/agents-go/cmd/agents-server/internal/mcpservers"
	"github.com/zzir/agents-go/cmd/agents-server/internal/protocol"
	"github.com/zzir/agents-go/cmd/agents-server/internal/providers"
	"github.com/zzir/agents-go/cmd/agents-server/internal/sandboxes"
	"github.com/zzir/agents-go/cmd/agents-server/internal/server"
	"github.com/zzir/agents-go/cmd/agents-server/internal/settings"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
	"github.com/zzir/agents-go/cmd/agents-server/internal/web"
)

// The composition root, in the order the layers stack: stores on the
// database, the bridge on the stores, handlers on both, then auth and the
// server that mounts it all. run (root.go) calls these in sequence and keeps
// the start-up and shutdown ordering that spans them.

// stores is every table's store, on one database.
type stores struct {
	Sessions         *store.SessionStore
	Entries          *store.EntryStore
	Traces           *store.TraceStore
	AgentConfigs     *store.AgentConfigStore
	McpServers       *store.McpServerStore
	Memories         *store.MemoryStore
	Settings         *store.SettingStore
	SettingReader    *settings.Reader
	Sandboxes        *store.SandboxStore
	Guardrails       *store.GuardrailStore
	PendingApprovals *store.PendingApprovalStore
	Tasks            *store.TaskStore
	Providers        *store.ProviderStore
	Workflows        *store.WorkflowStore
	Triggers         *store.TriggerStore
	Wakeups          *store.WakeupStore
	Users            *store.UserStore
	AuthTokens       *store.AuthTokenStore
	ContextProfiles  *store.ContextProfileStore
	Audit            *store.AuditStore
}

func newStores(db *bun.DB) *stores {
	settingStore := store.NewSettingStore(db)
	settingStore.SealIf(settings.IsSecret)
	return &stores{
		Sessions:         store.NewSessionStore(db),
		Entries:          store.NewSharedEntryStore(db),
		Traces:           store.NewTraceStore(db),
		AgentConfigs:     store.NewAgentConfigStore(db),
		McpServers:       store.NewMcpServerStore(db),
		Memories:         store.NewMemoryStore(db),
		Settings:         settingStore,
		SettingReader:    settings.NewReader(settingStore),
		Sandboxes:        store.NewSandboxStore(db),
		Guardrails:       store.NewGuardrailStore(db),
		PendingApprovals: store.NewPendingApprovalStore(db),
		Tasks:            store.NewTaskStore(db),
		Providers:        store.NewProviderStore(db),
		Workflows:        store.NewWorkflowStore(db),
		Triggers:         store.NewTriggerStore(db),
		Wakeups:          store.NewWakeupStore(db),
		Users:            store.NewUserStore(db),
		AuthTokens:       store.NewAuthTokenStore(db),
		ContextProfiles:  store.NewContextProfileStore(db),
		Audit:            store.NewAuditStore(db),
	}
}

// auditRecorder persists one audit line; a failed write is logged, never
// surfaced — the request it records has already been answered.
func auditRecorder(audit *store.AuditStore, log *slog.Logger) protocol.AuditFunc {
	return func(ctx context.Context, r protocol.AuditRecord) {
		err := audit.Record(ctx, &store.AuditEvent{
			ActorID: r.Actor.ID, ActorEmail: r.Actor.Email,
			Action: r.Action, Resource: r.Resource, Detail: r.Detail, ClientIP: r.ClientIP,
		})
		if err != nil {
			log.Error("audit record failed", "error", err, "action", r.Action)
		}
	}
}

// services is the bridge layer: what runs, connects and keeps time.
type services struct {
	Runner     *bridge.Runner
	Deps       *bridge.AgentDeps
	Mcp        *mcpservers.Manager
	OAuth      *mcpservers.OAuthCoordinator
	ChatGPT    *providers.ChatGPTOAuth
	Sandboxes  *sandboxes.Manager
	Guardrails *guardrails.Resolver
	Scheduler  *bridge.TriggerScheduler
}

// newBridge builds the bridge on the stores and starts the two loops that
// need nothing above it: MCP auto-connect and trace retention, on bgCtx.
func newBridge(ctx, bgCtx context.Context, db *bun.DB, st *stores, audit protocol.AuditFunc) *services {
	svc := &services{
		Guardrails: guardrails.NewResolver(st.Guardrails),
		Mcp:        mcpservers.NewManager(ctx, st.SettingReader),
		OAuth:      mcpservers.NewOAuthCoordinator(st.McpServers),
		ChatGPT:    providers.NewChatGPTOAuth(st.Providers, st.SettingReader),
		Sandboxes:  sandboxes.NewManager(flagWorkspace),
	}
	go mcpservers.ConnectEnabled(bgCtx, svc.Mcp, st.McpServers, svc.OAuth)
	go bridge.RunTraceRetention(bgCtx, st.SettingReader, st.Traces)
	svc.Deps = &bridge.AgentDeps{
		AgentConfigs:     st.AgentConfigs,
		Providers:        st.Providers,
		McpServers:       st.McpServers,
		SandboxConfigs:   st.Sandboxes,
		Memories:         st.Memories,
		Settings:         st.SettingReader,
		Sessions:         st.Sessions,
		Traces:           st.Traces,
		Guardrails:       svc.Guardrails,
		McpManager:       svc.Mcp,
		SandboxManager:   svc.Sandboxes,
		ChatGPTOAuth:     svc.ChatGPT,
		PendingApprovals: st.PendingApprovals,
		Tasks:            st.Tasks,
		ContextProfiles:  st.ContextProfiles,
		Workflows:        st.Workflows,
		Users:            st.Users,
		Wakeups:          st.Wakeups,
		Workspace:        flagWorkspace,
		MaxTasks:         flagMaxTasks,
		Audit:            audit,
	}
	svc.Runner = bridge.NewRunner(ctx, db, svc.Deps)
	svc.Scheduler = bridge.NewTriggerScheduler(svc.Runner, st.Triggers)
	return svc
}

// Close releases what the bridge holds open: MCP connections and sandbox
// instances. The runner and the scheduler are stopped by run's shutdown
// sequence, which orders them against the drain.
func (s *services) Close() {
	s.Mcp.CloseAll()
	s.Sandboxes.CloseAll()
}

// handlers is every handler the server mounts: the REST set, and the two
// WebSocket endpoints that register separately.
type handlers struct {
	API      handler.Handlers
	WS       *handler.WSHandler
	Terminal *handler.TerminalHandler
}

// newHandlers builds the handlers on the stores and the bridge. Handlers.Auth
// is left for newServer: it needs the auth service and the server's
// connection registry.
func newHandlers(st *stores, svc *services, audit protocol.AuditFunc, baseURL, workspaceAbs string) *handlers {
	terminal := handler.NewTerminalHandler(st.Sandboxes, svc.Sandboxes, st.SettingReader)
	terminal.Audit = audit
	ws := handler.NewWSHandler(svc.Runner, st.Sessions, st.PendingApprovals)
	ws.Audit = audit
	return &handlers{
		WS:       ws,
		Terminal: terminal,
		API: handler.Handlers{
			Authz: handler.AuthzDeps{
				Sessions: st.Sessions, Tasks: st.Tasks, Approvals: st.PendingApprovals,
				Triggers: st.Triggers, Hub: svc.Runner.Hub(),
			},
			Sessions: handler.NewSessionHandler(handler.SessionDeps{
				Sessions: st.Sessions, Entries: st.Entries, Traces: st.Traces, Agents: st.AgentConfigs,
				Profiles: st.ContextProfiles, MCP: svc.Mcp, MCPServers: st.McpServers, Users: st.Users,
				Stopper: svc.Runner, Compactor: svc.Runner,
			}),
			Runs:       handler.NewRunHandler(svc.Runner),
			Approvals:  handler.NewApprovalHandler(st.PendingApprovals, svc.Runner),
			Tasks:      handler.NewTaskHandler(st.Tasks, svc.Runner),
			Agents:     handler.NewAgentConfigHandler(st.AgentConfigs, st.McpServers, st.Providers, svc.Guardrails),
			McpServers: handler.NewMcpServerHandler(st.McpServers, svc.Mcp, svc.OAuth, baseURL),
			Memories:   handler.NewMemoryHandler(st.Memories),
			Settings:   handler.NewSettingHandler(st.Settings),
			Skills:     handler.NewSkillHandler(flagWorkspace),
			Providers:  handler.NewProviderHandler(st.Providers),
			Workflows:  handler.NewWorkflowHandler(st.Workflows, st.AgentConfigs, st.Sessions, svc.Runner),
			Triggers:   handler.NewTriggerHandler(st.Triggers, st.Sessions, svc.Scheduler),
			Guardrails: handler.NewGuardrailHandler(st.Guardrails, svc.Guardrails),
			Sandboxes:  handler.NewSandboxHandler(st.Sandboxes, svc.Sandboxes, flagAllowLocalSandbox, terminal, flagWorkspace),
			Traces:     handler.NewTraceHandler(st.Traces),
			Playground: handler.NewPlaygroundHandler(svc.Deps),
			ChatGPT:    handler.NewChatGPTOAuthHandler(svc.ChatGPT),
			Server: handler.ServerInfo{
				Version:           buildVersion,
				Workspace:         workspaceAbs,
				AllowLocalSandbox: flagAllowLocalSandbox,
				MaxTasks:          svc.Runner.Hub().MaxTasks(),
			},
		},
	}
}

// newAuth builds the auth service the --auth mode names. Both modes keep the
// implicit local account, so ownership always has a referent — token mode
// authenticates as it, OAuth mode leaves it dormant (no identity, no token,
// no way to sign in as it). OAuth mode fails fast on a combination that could
// not be signed in to, or that would admit everyone.
func newAuth(ctx context.Context, st *stores, baseURL string, log *slog.Logger) (*authn.Service, error) {
	localUser, err := st.Users.EnsureLocalUser(ctx)
	if err != nil {
		return nil, fmt.Errorf("ensuring the local user: %w", err)
	}
	switch flagAuthMode {
	case "token":
		token := flagToken
		if token == "" {
			token = server.GenerateToken()
		}
		log.Info("auth token", "token", token)
		return authn.NewStatic(token, localUser), nil
	case "oauth":
		if flagToken != "" {
			return nil, fmt.Errorf("--token cannot combine with --auth oauth; programmatic access uses personal access tokens")
		}
		if baseURL == "" {
			return nil, fmt.Errorf("--auth oauth requires --base-url: the OAuth redirect URI derives from it")
		}
		googleSecret := flagGoogleSecret
		if googleSecret == "" {
			googleSecret = os.Getenv("AGENTS_OAUTH_GOOGLE_CLIENT_SECRET")
		}
		var oauthProviders []authn.OAuthProvider
		if flagGoogleClientID != "" {
			if googleSecret == "" {
				return nil, fmt.Errorf("google login needs --oauth-google-client-secret (or AGENTS_OAUTH_GOOGLE_CLIENT_SECRET)")
			}
			oauthProviders = append(oauthProviders, &authn.Google{ClientID: flagGoogleClientID, ClientSecret: googleSecret})
		}
		if len(oauthProviders) == 0 {
			return nil, fmt.Errorf("--auth oauth needs at least one provider (--oauth-google-client-id)")
		}
		domains, emails := splitList(flagAllowedDomains), splitList(flagAllowedEmails)
		bootstrapAdmin := strings.ToLower(strings.TrimSpace(flagBootstrapAdmin))
		for _, d := range domains {
			if strings.Contains(d, "@") {
				return nil, fmt.Errorf("--allowed-domains takes domains, not addresses: %q", d)
			}
		}
		for _, e := range append(emails, bootstrapAdmin) {
			if e != "" && !strings.Contains(e, "@") {
				return nil, fmt.Errorf("--allowed-emails and --bootstrap-admin take addresses: %q", e)
			}
		}
		if flagBootstrapAdmin != "" && bootstrapAdmin == "" {
			return nil, fmt.Errorf("--bootstrap-admin is blank")
		}
		if len(domains) == 0 && len(emails) == 0 && bootstrapAdmin == "" {
			return nil, fmt.Errorf("--auth oauth needs an allowlist: --allowed-domains, --allowed-emails or --bootstrap-admin")
		}
		if bootstrapAdmin == "" {
			// Without a named admin the first account to sign in is it — a
			// race anyone on an allowed domain can win until someone has.
			if n, err := st.Users.CountReal(ctx); err != nil {
				return nil, fmt.Errorf("counting users: %w", err)
			} else if n == 0 {
				log.Warn("no admin account yet: the first OAuth login becomes the admin — name one with --bootstrap-admin to choose who")
			}
		}
		return authn.NewOAuth(authn.OAuthConfig{
			Users: st.Users, Tokens: st.AuthTokens,
			BaseURL: baseURL, Providers: oauthProviders,
			AllowedDomains: domains, AllowedEmails: emails,
			BootstrapAdmin: bootstrapAdmin, Log: log,
		}), nil
	default:
		return nil, fmt.Errorf("unknown --auth mode %q (token or oauth)", flagAuthMode)
	}
}

// newServer builds the HTTP server and mounts everything on it: the auth
// handler (built here, on the server's connection registry), the REST API,
// the WebSocket endpoints, the webhook hook, health, the OpenAPI document and
// the embedded SPA.
func newServer(ctx context.Context, log *slog.Logger, authSvc *authn.Service, audit protocol.AuditFunc, st *stores, hs *handlers, baseURL string) (*server.Server, error) {
	srv := server.New(log, authSvc.Authenticate, audit)
	srv.SetImageHosts(authSvc.AvatarHosts())
	// A replay posts a stored span payload back: the body cap follows the
	// size the settings let a span keep, plus room for the rest of the request.
	srv.SetBodyLimit(server.APIPrefix+"/playground/generate", func() int64 {
		return int64(st.SettingReader.Int(ctx, settings.KeyTraceSpanDataKB))*1024 + 256*1024
	})
	if flagTrustedProxies != "" {
		if err := srv.SetTrustedProxies(splitList(flagTrustedProxies)); err != nil {
			return nil, fmt.Errorf("invalid --trusted-proxies: %w", err)
		}
	} else if baseURL != "" {
		// --base-url is the "behind a proxy" signal; without trusted proxies
		// every client shares the proxy's IP and so one per-IP budget.
		log.Warn("--base-url is set but --trusted-proxies is not: every request arrives from the proxy's address, so all users share one per-IP rate budget")
	}
	authHandler := handler.NewAuthHandler(authSvc, st.AuthTokens, st.Users, st.Audit)
	authHandler.Conns = srv.Conns
	hs.API.Auth = authHandler
	srv.RegisterAPI(hs.API.Register)
	srv.RegisterWS(hs.WS.Handle, hs.Terminal.Handle)
	srv.RegisterHook(hs.API.Triggers.Hook)
	srv.ServeHealth(buildVersion)
	srv.ServeOpenAPI(docs.SpecYAML)
	staticFS, err := fs.Sub(web.StaticFS, "frontend/dist")
	if err != nil {
		return nil, fmt.Errorf("embedding static files: %w", err)
	}
	srv.ServeStatic(staticFS)
	return srv, nil
}
