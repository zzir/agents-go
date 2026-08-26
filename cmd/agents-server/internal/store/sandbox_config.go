package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// This file is the single home of sandbox config SEMANTICS: what a payload
// must contain to be storable (NormalizeSandboxConfig), when two payloads
// mean the same runtime content (ContentEqual), and when an update moves the
// sandbox's identity (IdentityChanged). All three decode through the same
// typed config and the same canonicalizer, so a question answered one way at
// save time cannot be answered another way at compare time.

// NormalizeSandboxConfig strictly decodes raw ("docker" is the only backend
// — decisions §5.27), enforces the fields the sandbox cannot run without, and
// returns the canonical payload to store. It gates the API write path: a
// payload that decodes here builds later — a stored type mismatch would
// otherwise read as its zero value at build time. Canonical means fields
// re-marshaled in struct order and unknown keys dropped (nothing consumes
// them).
func NormalizeSandboxConfig(typ string, raw json.RawMessage) (json.RawMessage, error) {
	if typ != "docker" {
		return nil, fmt.Errorf("sandbox type must be docker, got %q", typ)
	}
	var dc DockerConfig
	if err := unmarshalConfigJSON(raw, &dc); err != nil {
		return nil, fmt.Errorf("docker config: %w", err)
	}
	if dc.Image == "" {
		return nil, errors.New("docker sandbox requires config.image")
	}
	switch {
	case dc.Host == "" || strings.HasPrefix(dc.Host, "tcp://"):
	case strings.HasPrefix(dc.Host, "ssh://"):
		if !strings.Contains(strings.TrimPrefix(dc.Host, "ssh://"), "@") {
			return nil, errors.New("an ssh:// host must carry its user: ssh://user@host")
		}
	default:
		return nil, errors.New("host must be empty (local daemon), tcp://host:port, or ssh://user@host")
	}
	if dc.MaxReadFileBytes < 0 {
		return nil, errors.New("max_read_file_bytes cannot be negative")
	}
	if dc.MemoryMB < 0 || dc.CPUs < 0 {
		return nil, errors.New("memory_mb and cpus cannot be negative")
	}
	return json.Marshal(dc)
}

// ContentEqual reports whether two sandbox config payloads mean the same
// runtime CONTENT — the predicate behind contentChanged (RuntimeGen bump,
// instance retirement). Canonical typed comparison keeps representation
// noise — omitted-vs-zero fields, unknown keys — from tearing down a
// container; a payload that cannot decode compares UNEQUAL, the safe side.
func ContentEqual(_ string, a, b json.RawMessage) bool {
	return canonicalEqual(a, b, func(*DockerConfig) {})
}

// IdentityChanged reports whether an update moves the sandbox's IDENTITY —
// type and daemon Host, the fields that decide where a binding's files live;
// they freeze while sessions bind the config (workbench invariant 27). An
// undecodable prev is NOT a change — fixing it is a bound session's only way
// out; an undecodable next counts as one, pure defense.
func IdentityChanged(prev, next *SandboxConfig) bool {
	if prev.Type != next.Type {
		return true
	}
	var p, n DockerConfig
	if unmarshalConfigJSON(prev.Config, &p) != nil {
		return false
	}
	if unmarshalConfigJSON(next.Config, &n) != nil {
		return true
	}
	return p.Host != n.Host
}

// canonicalEqual compares two raw payloads through T, canonicalized. The
// comparable constraint is deliberate: it keeps every field of the config
// struct a plain value, so adding a slice or map field breaks the build
// here and forces a decision about how it compares instead of silently
// changing semantics.
func canonicalEqual[T comparable](a, b json.RawMessage, canon func(*T)) bool {
	var va, vb T
	if unmarshalConfigJSON(a, &va) != nil || unmarshalConfigJSON(b, &vb) != nil {
		return false
	}
	canon(&va)
	canon(&vb)
	return va == vb
}

// unmarshalConfigJSON fills dst from raw; an absent payload is the zero
// config. Unknown keys are ignored (see ContentEqual for why), a type
// mismatch on a known key is an error.
func unmarshalConfigJSON[T any](raw json.RawMessage, dst *T) error {
	if len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, dst)
}
