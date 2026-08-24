package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// This file is the single home of sandbox config SEMANTICS: what a payload
// must contain to be storable (NormalizeSandboxConfig), when two payloads
// mean the same runtime content (ContentEqual), and when an update moves the
// sandbox's identity (IdentityChanged). All three decode through the same
// typed config and the same canonicalizer, so a question answered one way at
// save time cannot be answered another way at compare time.

// NormalizeSandboxConfig strictly decodes raw ("docker" is the only backend
// — spec §5.27), enforces the fields the sandbox cannot run without, and
// returns the canonical payload to store. It gates the API write path: a
// payload that decodes here builds later, where a stored type mismatch
// (persistent:"yes") would read as its zero value at binding time and
// permanently bind a session to a config that can never build. Canonical
// means fields re-marshaled in struct order, paths cleaned of trailing
// slashes, and unknown keys dropped (nothing consumes them).
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
	canonicalizeDocker(&dc)
	return json.Marshal(dc)
}

// ContentEqual reports whether two sandbox config payloads mean the same
// runtime CONTENT — the predicate behind contentChanged, which bumps
// RuntimeGen, retires live instances and severs web terminals. Typed decoding
// plus canonicalization keeps representation noise from counting as a change:
// an omitted field equals its explicit zero (the UI round-trips every field
// with explicit zeros — compared as maps, a mere rename of a
// minimally-written config would tear down a docker container), and unknown
// keys are ignored. A payload that cannot decode compares unequal on the safe
// side: retiring too much rebuilds an environment, while the opposite miss
// keeps old credentials serving. A TYPE change is a content change by
// definition; callers short-circuit it before asking here.
func ContentEqual(_ string, a, b json.RawMessage) bool {
	return canonicalEqual(a, b, canonicalizeDocker)
}

// IdentityChanged reports whether an update moves the sandbox's IDENTITY —
// the fields that decide where a binding's files live: the backend type, the
// DAEMON the containers run on (Host — a different daemon is a different set
// of filesystems even under identical mount spellings), the mount source,
// mode and adopted container. Sessions bind a config id permanently on the
// promise that it keeps meaning the same file system, so these fields freeze
// while any session references the config; everything else — name, SSH
// credentials, image, network, runtime, limits, the exec user — changes the
// execution environment, not where the data is, and stays freely editable.
//
// A prev that no longer decodes is NOT a change: sessions bound to it hold a
// config that cannot build, and fixing it is their only way out. (An
// undecodable NEXT counts as one — pure defense; normalization upstream
// refuses those payloads.)
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
	canonicalizeDocker(&p)
	canonicalizeDocker(&n)
	return p.Host != n.Host || p.HostDir != n.HostDir || p.Persistent != n.Persistent || p.ContainerName != n.ContainerName
}

// canonicalizeDocker rewrites a docker config's path spellings to canonical
// form. The host dir is a HOST path (filepath semantics).
func canonicalizeDocker(dc *DockerConfig) {
	if dc.HostDir != "" {
		dc.HostDir = filepath.Clean(dc.HostDir)
	}
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
