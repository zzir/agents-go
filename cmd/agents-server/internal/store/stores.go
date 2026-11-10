package store

import "github.com/uptrace/bun"

// Stores is every table's store, on one database.
type Stores struct {
	Sessions         *SessionStore
	Traces           *TraceStore
	AgentConfigs     *AgentConfigStore
	McpServers       *McpServerStore
	Skills           *SkillStore
	Memories         *MemoryStore
	Settings         *SettingStore
	Sandboxes        *SandboxStore
	Workspaces       *WorkspaceStore
	Guardrails       *GuardrailStore
	PendingApprovals *PendingApprovalStore
	Tasks            *TaskStore
	Providers        *ProviderStore
	Workflows        *WorkflowStore
	Triggers         *TriggerStore
	Wakeups          *WakeupStore
	Users            *UserStore
	AuthTokens       *AuthTokenStore
	ContextProfiles  *ContextProfileStore
	Audit            *AuditStore
	Attachments      *AttachmentStore
}

// NewStores builds every store on db.
func NewStores(db *bun.DB) *Stores {
	return &Stores{
		Sessions:         NewSessionStore(db),
		Traces:           NewTraceStore(db),
		AgentConfigs:     NewAgentConfigStore(db),
		McpServers:       NewMcpServerStore(db),
		Skills:           NewSkillStore(db),
		Memories:         NewMemoryStore(db),
		Settings:         NewSettingStore(db),
		Sandboxes:        NewSandboxStore(db),
		Workspaces:       NewWorkspaceStore(db),
		Guardrails:       NewGuardrailStore(db),
		PendingApprovals: NewPendingApprovalStore(db),
		Tasks:            NewTaskStore(db),
		Providers:        NewProviderStore(db),
		Workflows:        NewWorkflowStore(db),
		Triggers:         NewTriggerStore(db),
		Wakeups:          NewWakeupStore(db),
		Users:            NewUserStore(db),
		AuthTokens:       NewAuthTokenStore(db),
		ContextProfiles:  NewContextProfileStore(db),
		Audit:            NewAuditStore(db),
		Attachments:      NewAttachmentStore(db),
	}
}
