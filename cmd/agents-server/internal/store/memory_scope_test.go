package store

import "github.com/zzir/agents-go/agents/memory"

// memoryScopeOf is the SDK-side scope a run hands the adapter.
func memoryScopeOf(kind, id string) memory.Scope { return memory.Scope{Kind: kind, ID: id} }
