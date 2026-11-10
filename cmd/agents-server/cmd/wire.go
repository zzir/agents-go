package cmd

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"net/url"
	"time"

	"github.com/uptrace/bun"

	"github.com/zzir/agents-go/cmd/agents-server/internal/attachments"
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

// The composition root, in the order the layers stack: stores, bridge,
// handlers, then auth and the server; run (root.go) calls these in sequence.

// stores is every table's store on one database, with the settings reader
// over it.
type stores struct {
	*store.Stores
	SettingReader *settings.Reader
}

func newStores(db *bun.DB) *stores {
	st := store.NewStores(db)
	st.Settings.SealIf(settings.IsSecret)
	return &stores{Stores: st, SettingReader: settings.NewReader(st.Settings)}
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
	Mcp        *mcpservers.Manager
	OAuth      *mcpservers.OAuthCoordinator
	ChatGPT    *providers.ChatGPTOAuth
	Sandboxes  *sandboxes.Manager
	Guardrails *guardrails.Resolver
	Scheduler  *bridge.TriggerScheduler
}

// newBridge builds the bridge on the stores and starts the two loops that
// need nothing above it: MCP auto-connect and trace retention, on bgCtx.
func newBridge(ctx, bgCtx context.Context, db *bun.DB, st *stores, audit protocol.AuditFunc) (*services, error) {
	svc := &services{
		Guardrails: guardrails.NewResolver(st.Guardrails),
		Mcp:        mcpservers.NewManager(ctx),
		OAuth:      mcpservers.NewOAuthCoordinator(st.McpServers),
		ChatGPT:    providers.NewChatGPTOAuth(st.Providers),
		Sandboxes:  sandboxes.NewManager(),
	}
	svc.Sandboxes.SetInstanceRefWriter(st.Workspaces.SetInstanceRef)
	svc.Sandboxes.SetIdleTimeout(func() time.Duration {
		return time.Duration(st.SettingReader.Int(context.Background(), settings.KeySandboxIdleMinutes)) * time.Minute
	})
	deps := &bridge.AgentDeps{
		Stores:            st.Stores,
		SettingReader:     st.SettingReader,
		GuardrailResolver: svc.Guardrails,
		McpManager:        svc.Mcp,
		SandboxManager:    svc.Sandboxes,
		ChatGPTOAuth:      svc.ChatGPT,
		RecordAudit:       audit,
	}
	// Refuses an incomplete dependency set — invariant 104.
	var err error
	if svc.Runner, err = bridge.NewRunner(ctx, db, deps); err != nil {
		return nil, err
	}
	svc.Scheduler = bridge.NewTriggerScheduler(svc.Runner, st.Triggers)
	go mcpservers.ConnectEnabled(bgCtx, svc.Mcp, st.McpServers, svc.OAuth)
	go bridge.RunTraceRetention(bgCtx, st.SettingReader, st.Traces)
	return svc, nil
}

// Close releases what the bridge holds open: MCP connections and sandbox
// instances; the runner and the scheduler are stopped by run's shutdown sequence.
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

// newHandlers builds the handlers on the stores and the bridge; info is the
// process facts /server reports. Handlers.Auth is left for newServer.
func newHandlers(st *stores, svc *services, audit protocol.AuditFunc, baseURL string, info handler.ServerInfo) *handlers {
	terminal := handler.NewTerminalHandler(st.Sandboxes, st.Workspaces, svc.Sandboxes, st.SettingReader)
	terminal.Audit = audit
	ws := handler.NewWSHandler(svc.Runner, st.Sessions, st.PendingApprovals)
	ws.Audit = audit
	workspaces := handler.NewWorkspaceHandler(st.Workspaces, st.Sandboxes, svc.Sandboxes, terminal, st.SettingReader)
	workspaces.Audit = audit
	workspaces.Notes = &handler.RebuildNoteDeps{Sessions: st.Sessions, Fence: svc.Runner}
	workspaces.StopCommands = svc.Runner.StopWorkspaceCommands
	return &handlers{
		WS:       ws,
		Terminal: terminal,
		API: handler.Handlers{
			Authz: handler.AuthzDeps{
				Sessions: st.Sessions, Tasks: st.Tasks, Approvals: st.PendingApprovals,
				Triggers: st.Triggers, Hub: svc.Runner.Hub(),
			},
			Sessions: handler.NewSessionHandler(handler.SessionDeps{
				Sessions: st.Sessions, Traces: st.Traces, Agents: st.AgentConfigs,
				Profiles: st.ContextProfiles, Users: st.Users,
				Workspaces: st.Workspaces, Stopper: svc.Runner, Compactor: svc.Runner, Statuses: svc.Runner,
				Settings: st.SettingReader,
			}),
			Runs:       handler.NewRunHandler(svc.Runner),
			Approvals:  handler.NewApprovalHandler(st.PendingApprovals, svc.Runner),
			Tasks:      handler.NewTaskHandler(st.Tasks, svc.Runner),
			Agents:     handler.NewAgentConfigHandler(st.AgentConfigs, st.McpServers, st.Providers, st.Skills, svc.Guardrails),
			McpServers: handler.NewMcpServerHandler(st.McpServers, svc.Mcp, svc.OAuth, baseURL),
			Memories:   handler.NewMemoryHandler(st.Memories, st.Sessions, st.AgentConfigs),
			Settings:   handler.NewSettingHandler(st.Settings),
			Skills:     handler.NewSkillHandler(st.Skills),
			Providers:  handler.NewProviderHandler(st.Providers, svc.ChatGPT),
			Workflows:  handler.NewWorkflowHandler(st.Workflows, st.AgentConfigs, st.Sessions, svc.Runner),
			Triggers:   handler.NewTriggerHandler(st.Triggers, st.Sessions, st.Workflows, st.AgentConfigs, svc.Scheduler),
			Guardrails: handler.NewGuardrailHandler(st.Guardrails, svc.Guardrails),
			Sandboxes:  handler.NewSandboxHandler(st.Sandboxes, st.Workspaces, svc.Sandboxes, terminal),
			Workspaces: workspaces,
			Traces:     handler.NewTraceHandler(st.Traces, st.SettingReader),
			Playground: handler.NewPlaygroundHandler(svc.Runner),
			Files:      handler.NewAttachmentHandler(st.Attachments, st.SettingReader, st.Settings),
			Server:     info,
		},
	}
}

// newAuth builds the auth service cfg's --auth mode names; both modes keep the
// implicit local account (token mode signs in as it, OAuth mode leaves it dormant).
func newAuth(ctx context.Context, st *stores, cfg *config, log *slog.Logger) (*authn.Service, error) {
	localUser, err := st.Users.EnsureLocalUser(ctx)
	if err != nil {
		return nil, fmt.Errorf("ensuring the local user: %w", err)
	}
	auth := cfg.Auth
	if auth.Mode == authn.ModeToken {
		token := auth.Token
		if token == "" {
			// Logged only when generated.
			token = server.GenerateToken()
			log.Info("auth token", "token", token)
		}
		return authn.NewStatic(token, localUser), nil
	}
	if auth.BootstrapAdmin == "" {
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
		BaseURL: cfg.BaseURL, Providers: auth.Providers,
		AllowedDomains: auth.AllowedDomains, AllowedEmails: auth.AllowedEmails,
		BootstrapAdmin: auth.BootstrapAdmin, Log: log,
	}), nil
}

// newServer builds the HTTP server and mounts everything on it: the auth
// handler, the REST API, the WebSocket endpoints, the hook, health, OpenAPI and
// the SPA.
func newServer(ctx context.Context, log *slog.Logger, authSvc *authn.Service, audit protocol.AuditFunc, st *stores, hs *handlers, cfg *config) (*server.Server, error) {
	srv := server.New(log, authSvc.Authenticate, audit)
	// img-src admits the avatar hosts plus the attachment bucket's host,
	// re-applied whenever a storage key changes (invariant 58).
	imgHosts := func() []string {
		hosts := authSvc.AvatarHosts()
		if cfg := st.SettingReader.S3Config(ctx); cfg.Complete() {
			if u, err := url.Parse(cfg.PublicBaseURL); err == nil && u.Host != "" {
				hosts = append(hosts, u.Scheme+"://"+u.Host)
			}
		}
		return hosts
	}
	srv.SetImageHosts(imgHosts())
	hs.API.Files.OnStorageChange = func() { srv.SetImageHosts(imgHosts()) }
	// A replay posts a stored span payload back, well past the 1 MiB default.
	srv.SetBodyLimit(server.APIPrefix+"/playground/generate", handler.MaxReplayBodyBytes)
	// An image upload legitimately exceeds the 1 MiB default body cap: the
	// image plus the multipart framing around it.
	srv.SetBodyLimit(server.APIPrefix+"/attachments", attachments.MaxBytes+64<<10)
	if err := trustProxies(srv, cfg, log); err != nil {
		return nil, err
	}
	authHandler := handler.NewAuthHandler(authSvc, st.AuthTokens, st.Users, st.Audit)
	authHandler.Conns = srv.Conns
	hs.API.Auth = authHandler
	srv.RegisterAPI(hs.API.Register)
	srv.RegisterWS(hs.WS.Handle, hs.Terminal.Handle)
	srv.RegisterHook(hs.API.Hook())
	srv.ServeHealth(buildVersion)
	srv.ServeOpenAPI(docs.SpecYAML)
	staticFS, err := fs.Sub(web.StaticFS, "frontend/dist")
	if err != nil {
		return nil, fmt.Errorf("embedding static files: %w", err)
	}
	srv.ServeStatic(staticFS)
	return srv, nil
}

// trustProxies applies --trusted-proxies to srv, refusing an entry that is
// neither an IP nor a CIDR.
func trustProxies(srv *server.Server, cfg *config, log *slog.Logger) error {
	if len(cfg.TrustedProxies) > 0 {
		if err := srv.SetTrustedProxies(cfg.TrustedProxies); err != nil {
			return fmt.Errorf("invalid --trusted-proxies: %w", err)
		}
	} else if cfg.BaseURL != "" {
		// --base-url is the "behind a proxy" signal; without trusted proxies
		// every client shares the proxy's IP and so one per-IP budget.
		log.Warn("--base-url is set but --trusted-proxies is not: every request arrives from the proxy's address, so all users share one per-IP rate budget")
	}
	return nil
}
