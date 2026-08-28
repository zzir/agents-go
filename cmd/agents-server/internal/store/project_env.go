package store

import (
	"cmp"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
)

// This file is the single home of project ENVIRONMENT semantics: what is
// storable (NormalizeProjectEnv), what reaches the container (EnvMap), and
// when two payloads mean the same container (EnvContentEqual) — so a question
// answered one way at save time cannot be answered another way at compare
// time.

// EnvVar is one entry of a project's environment. Values are write-only:
// sealed at rest and masked in every response, like every other credential
// this server stores (decisions §5.32).
type EnvVar struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// The environment's bounds. The whole set rides along on every container
// create, so these keep that payload sane rather than the column small.
const (
	MaxEnvVars       = 64
	MaxEnvValueBytes = 32 << 10
	MaxEnvBytes      = 128 << 10
)

// envKeyPattern is the name a shell can actually export.
var envKeyPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// NormalizeProjectEnv validates vars and returns the canonical payload to
// store: sorted by key, so storage, the container fingerprint (spec §2.7n)
// and EnvContentEqual all answer in one order. An empty environment stores as
// "" — no environment at all, which is what keeps a project without one off
// the fingerprint.
func NormalizeProjectEnv(vars []EnvVar) (string, error) {
	if len(vars) == 0 {
		return "", nil
	}
	if len(vars) > MaxEnvVars {
		return "", fmt.Errorf("at most %d environment variables, got %d", MaxEnvVars, len(vars))
	}
	out := make([]EnvVar, 0, len(vars))
	seen := make(map[string]bool, len(vars))
	total := 0
	for _, v := range vars {
		if !envKeyPattern.MatchString(v.Key) {
			return "", fmt.Errorf("environment variable name %q must match [A-Za-z_][A-Za-z0-9_]*", v.Key)
		}
		// Refused, not deduplicated: silently dropping one of two values for
		// one name is a long debugging session.
		if seen[v.Key] {
			return "", fmt.Errorf("environment variable %q is set twice", v.Key)
		}
		seen[v.Key] = true
		if len(v.Value) > MaxEnvValueBytes {
			return "", fmt.Errorf("environment variable %q is %d bytes, over the %d-byte limit", v.Key, len(v.Value), MaxEnvValueBytes)
		}
		total += len(v.Key) + len(v.Value)
		out = append(out, v)
	}
	if total > MaxEnvBytes {
		return "", fmt.Errorf("the environment is %d bytes, over the %d-byte limit", total, MaxEnvBytes)
	}
	slices.SortFunc(out, func(a, b EnvVar) int { return cmp.Compare(a.Key, b.Key) })
	b, err := json.Marshal(out)
	if err != nil {
		return "", fmt.Errorf("encoding project environment: %w", err)
	}
	return string(b), nil
}

// DecodeProjectEnv reads a stored payload; an empty one is no environment.
func DecodeProjectEnv(raw string) ([]EnvVar, error) {
	if raw == "" {
		return nil, nil
	}
	var out []EnvVar
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, fmt.Errorf("decoding project environment: %w", err)
	}
	return out, nil
}

// EnvMap is the stored payload as the sandbox takes it. An undecodable
// payload is an error: starting a container WITHOUT the variables it was
// configured with is worse than refusing to start it.
func EnvMap(raw string) (map[string]string, error) {
	vars, err := DecodeProjectEnv(raw)
	if err != nil || len(vars) == 0 {
		return nil, err
	}
	out := make(map[string]string, len(vars))
	for _, v := range vars {
		out[v.Key] = v.Value
	}
	return out, nil
}

// EnvContentEqual reports whether two canonical payloads produce the same
// CONTAINER — the predicate behind the runtime-generation bump. An
// undecodable payload compares unequal, the safe side (as ContentEqual does
// for sandbox configs).
func EnvContentEqual(a, b string) bool {
	va, aerr := DecodeProjectEnv(a)
	vb, berr := DecodeProjectEnv(b)
	if aerr != nil || berr != nil {
		return false
	}
	return slices.EqualFunc(va, vb, func(x, y EnvVar) bool {
		return x.Key == y.Key && x.Value == y.Value
	})
}

// MaxProjectPorts bounds the published set: every one is a host-side binding
// the daemon holds for the container's whole life.
const MaxProjectPorts = 16

// NormalizeProjectPorts validates ports and returns the canonical payload to
// store: deduplicated and ordered, so storage, the container fingerprint
// (spec §2.7r) and PortsContentEqual all answer in one order. An empty list
// stores as "" — nothing published, which keeps a project without ports off
// the fingerprint.
func NormalizeProjectPorts(ports []int) (string, error) {
	if len(ports) == 0 {
		return "", nil
	}
	if len(ports) > MaxProjectPorts {
		return "", fmt.Errorf("at most %d published ports, got %d", MaxProjectPorts, len(ports))
	}
	out := make([]int, 0, len(ports))
	seen := make(map[int]bool, len(ports))
	for _, p := range ports {
		if p <= 0 || p > 65535 {
			return "", fmt.Errorf("port %d is out of range (1-65535)", p)
		}
		if seen[p] {
			continue // a repeated port is one binding, not an error
		}
		seen[p] = true
		out = append(out, p)
	}
	slices.Sort(out)
	b, err := json.Marshal(out)
	if err != nil {
		return "", fmt.Errorf("encoding project ports: %w", err)
	}
	return string(b), nil
}

// DecodeProjectPorts reads a stored payload; an empty one is no ports. An
// undecodable one is an error: publishing nothing where a port was configured
// silently breaks the preview that was the point of declaring it.
func DecodeProjectPorts(raw string) ([]int, error) {
	if raw == "" {
		return nil, nil
	}
	var out []int
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, fmt.Errorf("decoding project ports: %w", err)
	}
	return out, nil
}

// PortsContentEqual reports whether two canonical payloads produce the same
// CONTAINER. Semantics match EnvContentEqual's.
func PortsContentEqual(a, b string) bool {
	pa, aerr := DecodeProjectPorts(a)
	pb, berr := DecodeProjectPorts(b)
	if aerr != nil || berr != nil {
		return false
	}
	return slices.Equal(pa, pb)
}
