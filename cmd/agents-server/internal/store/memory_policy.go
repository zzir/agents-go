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

// Who may write a scope over the API, and whether the model may.
const (
	MemoryWriteAdmin        = "admin"
	MemoryWriteAgentEditor  = "agent_editor"
	MemoryWriteSessionOwner = "session_owner"

	ModelWriteNever   = ""
	ModelWriteFree    = "free"
	ModelWriteApprove = "approve"
)

// MemoryPolicy is one scope kind's rules: the matrix's only home (invariant 64).
type MemoryPolicy struct {
	// Injected renders the scope into the agent's instructions every request.
	Injected bool
	// Write names who may write over the API; ModelWrite whether the model
	// may: never, freely, or after approval.
	Write      string
	ModelWrite string
	// MaxBytes caps one memory's content, MaxKeys the memories of one scope.
	MaxBytes int
	MaxKeys  int
}

// MemoryPolicies is the matrix by scope kind.
var MemoryPolicies = map[string]MemoryPolicy{
	MemoryScopeGlobal:  {Injected: true, Write: MemoryWriteAdmin, ModelWrite: ModelWriteNever, MaxBytes: 8 << 10, MaxKeys: 64},
	MemoryScopeAgent:   {Injected: true, Write: MemoryWriteAgentEditor, ModelWrite: ModelWriteApprove, MaxBytes: 8 << 10, MaxKeys: 64},
	MemoryScopeSession: {Injected: false, Write: MemoryWriteSessionOwner, ModelWrite: ModelWriteFree, MaxBytes: 1_000_000, MaxKeys: 100},
}

// MemoryPolicyFor looks a kind up; ok is false for a kind the matrix lacks.
func MemoryPolicyFor(kind string) (MemoryPolicy, bool) {
	p, ok := MemoryPolicies[kind]
	return p, ok
}
