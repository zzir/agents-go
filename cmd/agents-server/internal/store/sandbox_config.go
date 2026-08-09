package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"path"
	"path/filepath"
)

// This file is the single home of sandbox config SEMANTICS: what a payload
// must contain to be storable (NormalizeSandboxConfig), when two payloads
// mean the same runtime content (ContentEqual), and when an update moves the
// sandbox's identity (IdentityChanged). All three decode through the same
// typed configs and the same canonicalizers, so a question answered one way
// at save time cannot be answered another way at compare time.

// NormalizeSandboxConfig strictly decodes raw for one backend type, enforces
// the fields the sandbox cannot run without (docker an image, ssh addr and
// user — mirroring buildSandbox's own checks), and returns the canonical
// payload to store. It gates the API write path: a payload that decodes here
// builds later, where a stored type mismatch (persistent:"yes") would read
// as its zero value at binding time and permanently bind a session to a
// config that can never build. Canonical means fields re-marshaled in struct
// order, an ssh addr carrying its port explicitly (host and host:22 are the
// same machine and must compare equal), paths cleaned of trailing slashes,
// and unknown keys dropped (nothing consumes them). An empty local config
// stays empty rather than becoming "{}".
func NormalizeSandboxConfig(typ string, raw json.RawMessage) (json.RawMessage, error) {
	switch typ {
	case "docker":
		var dc DockerConfig
		if err := unmarshalConfigJSON(raw, &dc); err != nil {
			return nil, fmt.Errorf("docker config: %w", err)
		}
		if dc.Image == "" {
			return nil, errors.New("docker sandbox requires config.image")
		}
		if dc.MaxReadFileBytes < 0 {
			return nil, errors.New("max_read_file_bytes cannot be negative")
		}
		canonicalizeDocker(&dc)
		return json.Marshal(dc)
	case "ssh":
		var sc SSHConfig
		if err := unmarshalConfigJSON(raw, &sc); err != nil {
			return nil, fmt.Errorf("ssh config: %w", err)
		}
		if sc.Addr == "" {
			return nil, errors.New("ssh sandbox requires config.addr")
		}
		if sc.User == "" {
			return nil, errors.New("ssh sandbox requires config.user")
		}
		if sc.MaxReadFileBytes < 0 {
			return nil, errors.New("max_read_file_bytes cannot be negative")
		}
		canonicalizeSSH(&sc)
		return json.Marshal(sc)
	default: // "local"; other types are refused upstream (validateSandbox)
		var lc LocalConfig
		if err := unmarshalConfigJSON(raw, &lc); err != nil {
			return nil, fmt.Errorf("local config: %w", err)
		}
		if lc.MaxReadFileBytes < 0 {
			return nil, errors.New("max_read_file_bytes cannot be negative")
		}
		if len(raw) == 0 {
			return nil, nil
		}
		return json.Marshal(lc)
	}
}

// ContentEqual reports whether two sandbox config payloads mean the same
// runtime CONTENT for one backend type — the predicate behind contentChanged,
// which bumps RuntimeGen, retires live instances and severs web terminals.
// Typed decoding plus canonicalization keeps representation noise from
// counting as a change: an omitted field equals its explicit zero (the UI
// round-trips every field with explicit zeros — compared as maps, a mere
// rename of a minimally-written config would tear down a docker container),
// host equals host:22, and unknown keys are ignored. A payload the type
// cannot decode compares unequal on the safe side: retiring too much
// rebuilds an environment, while the opposite miss keeps old credentials
// serving. A TYPE change is a content change by definition; callers
// short-circuit it before asking here.
func ContentEqual(typ string, a, b json.RawMessage) bool {
	switch typ {
	case "docker":
		return canonicalEqual(a, b, canonicalizeDocker)
	case "ssh":
		return canonicalEqual(a, b, canonicalizeSSH)
	default:
		return canonicalEqual(a, b, func(*LocalConfig) {})
	}
}

// IdentityChanged reports whether an update moves the sandbox's IDENTITY —
// the fields that decide where a binding's files live: the backend type; an
// ssh sandbox's machine, USER and default directory (the user picks the
// account, so user-a@host and user-b@host are different file systems even at
// one address); a docker sandbox's mount source, mode and adopted container.
// Sessions bind a config id permanently on the promise that it keeps meaning
// the same file system, so these fields freeze while any session references
// the config; everything else — name, credentials, image, network, runtime,
// limits, the docker exec user — changes the execution environment, not
// where the data is, and stays freely editable.
//
// Both sides are canonicalized first, so host → host:22 is not a move. A
// prev that no longer decodes is NOT a change: sessions bound to it hold a
// config that cannot build, and fixing it is their only way out. (An
// undecodable NEXT counts as one — pure defense; normalization upstream
// refuses those payloads.)
func IdentityChanged(prev, next *SandboxConfig) bool {
	if prev.Type != next.Type {
		return true
	}
	switch next.Type {
	case "ssh":
		var p, n SSHConfig
		if unmarshalConfigJSON(prev.Config, &p) != nil {
			return false
		}
		if unmarshalConfigJSON(next.Config, &n) != nil {
			return true
		}
		canonicalizeSSH(&p)
		canonicalizeSSH(&n)
		return p.Addr != n.Addr || p.User != n.User || p.WorkDir != n.WorkDir
	case "docker":
		var p, n DockerConfig
		if unmarshalConfigJSON(prev.Config, &p) != nil {
			return false
		}
		if unmarshalConfigJSON(next.Config, &n) != nil {
			return true
		}
		canonicalizeDocker(&p)
		canonicalizeDocker(&n)
		return p.HostDir != n.HostDir || p.Persistent != n.Persistent || p.ContainerName != n.ContainerName
	default:
		return false
	}
}

// canonicalizeDocker rewrites a docker config's path spellings to canonical
// form. The host dir is a HOST path (filepath semantics).
func canonicalizeDocker(dc *DockerConfig) {
	if dc.HostDir != "" {
		dc.HostDir = filepath.Clean(dc.HostDir)
	}
}

// canonicalizeSSH pins the addr's implicit default port and cleans the
// remote path (slash semantics — the remote is not this OS).
func canonicalizeSSH(sc *SSHConfig) {
	sc.Addr = ensureHostPort(sc.Addr)
	if sc.WorkDir != "" {
		sc.WorkDir = path.Clean(sc.WorkDir)
	}
}

// ensureHostPort appends ssh's default port when addr has none — the same
// normalization the ssh backend applies when dialing, applied here so the
// stored identity matches what is actually connected to.
func ensureHostPort(addr string) string {
	if addr == "" {
		return addr
	}
	if _, _, err := net.SplitHostPort(addr); err == nil {
		return addr
	}
	return net.JoinHostPort(addr, "22")
}

// canonicalEqual compares two raw payloads through T, canonicalized. The
// comparable constraint is deliberate: it keeps every field of the config
// structs a plain value, so adding a slice or map field breaks the build
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
