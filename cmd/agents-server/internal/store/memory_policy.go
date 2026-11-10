package store

// The memory scope kinds, and who wrote a row.
const (
	MemoryScopeGlobal  = "global"
	MemoryScopeAgent   = "agent"
	MemoryScopeSession = "session"

	MemoryWrittenByUser  = "user"
	MemoryWrittenByModel = "model"
)

// ApprovedPlanKey is the session memory an approved plan is kept under, and
// ApprovedPlanSource its Metadata; the workbench writes it, the model only
// reads it (invariant 87).
const (
	ApprovedPlanKey    = "approved-plan.md"
	ApprovedPlanSource = "plan_approval"
)

// ChecklistKey is the session memory a run's todo_write keeps its latest
// list under, and ChecklistSource its Metadata.
const (
	ChecklistKey    = "checklist.md"
	ChecklistSource = "checklist"
)

// Who may write a scope over the API.
const (
	MemoryWriteAdmin        = "admin"
	MemoryWriteAgentEditor  = "agent_editor"
	MemoryWriteSessionOwner = "session_owner"
)

// MemoryPolicy is one scope kind's API-write rule and limits (invariant 64).
type MemoryPolicy struct {
	// Write names who may write the scope over the API.
	Write string
	// MaxBytes caps one memory's content, MaxKeys the memories of one scope.
	MaxBytes int
	MaxKeys  int
}

// MemoryPolicies is the matrix by scope kind; a kind it lacks is no scope.
var MemoryPolicies = map[string]MemoryPolicy{
	MemoryScopeGlobal:  {Write: MemoryWriteAdmin, MaxBytes: 8 << 10, MaxKeys: 64},
	MemoryScopeAgent:   {Write: MemoryWriteAgentEditor, MaxBytes: 8 << 10, MaxKeys: 64},
	MemoryScopeSession: {Write: MemoryWriteSessionOwner, MaxBytes: 1_000_000, MaxKeys: 100},
}
