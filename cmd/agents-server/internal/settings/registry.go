// Package settings is the registry of the server's global configuration: one
// table naming every key, its type, default and how the panel presents it.
// Backend reads, secret masking, write validation and the settings panel all
// derive from it, so adding a global setting is one entry here.
package settings

import (
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// Kind is a setting's value type. It decides how a value is parsed and
// validated, and which control the panel renders.
type Kind string

// The kinds a setting's value may take.
const (
	KindString Kind = "string" // single-line free text
	KindText   Kind = "text"   // multi-line free text
	KindSecret Kind = "secret" // masked on read, sentinel-resolved on write
	KindInt    Kind = "int"
	KindBool   Kind = "bool"
)

// The keys. Every read, mask and validation names one of these rather than a
// string literal, so a rename is a compile error instead of a setting that
// silently stops being read.
const (
	KeyProxyURL                  = "proxy_url"
	KeySystemPrompt              = "system_prompt"
	KeyTraceRetentionDays        = "trace_retention_days"
	KeyTraceIncludeSensitiveData = "trace_include_sensitive_data"
	KeyTraceSpanDataKB           = "trace_span_data_kb"
	KeyLogSensitiveData          = "log_sensitive_data"
	KeyApprovalTTLMinutes        = "approval_ttl_minutes"
	KeyMaxTerminalsPerSandbox    = "max_terminals_per_sandbox"
	KeySandboxIdleMinutes        = "sandbox_idle_minutes"
	KeyPreviewEnabled            = "preview_enabled"
)

// The groups the panel renders as sections, in the order defs are listed.
const (
	GroupNetwork = "network"
	GroupPrompt  = "prompt"
	GroupTracing = "tracing"
	GroupLogging = "logging"
	GroupLimits  = "limits"
)

// Def is one global setting: what it is called, what it holds, what it means
// when unset, and how the panel presents it.
type Def struct {
	Key         string `json:"key"`
	Kind        Kind   `json:"kind"`
	Group       string `json:"group"`
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
	Placeholder string `json:"placeholder,omitempty"`
	// Default is the value that applies when the setting is unset. Empty means
	// the feature is off — never "the zero value happens to be right".
	Default string `json:"default,omitempty"`
	// Min and Max bound a KindInt value. A zero Max means unbounded.
	Min int `json:"min,omitempty"`
	Max int `json:"max,omitempty"`
	// Validate rejects a syntactically well-typed value the feature still
	// cannot use. Kind-level parsing runs first, so this only sees a value of
	// the right shape.
	Validate func(string) error `json:"-"`
}

// defs is the table. Order is panel order: network and prompt first (what
// every agent inherits), then the diagnostics and the caps.
var defs = []Def{{
	Key:         KeyProxyURL,
	Kind:        KindString,
	Group:       GroupNetwork,
	Label:       "Proxy URL",
	Placeholder: "http://127.0.0.1:7890 or socks5://127.0.0.1:1080",
	Description: "All outbound API and MCP HTTP requests will be routed through this proxy.",
	Validate:    validateProxyURL,
}, {
	Key:         KeySystemPrompt,
	Kind:        KindText,
	Group:       GroupPrompt,
	Label:       "System prompt",
	Placeholder: "Optional instructions prepended to all agents",
}, {
	// Defaulted: a generation span stores the whole conversation it was
	// given, so trace_events grows with the square of a session's length —
	// "keep everything" is a choice, not the absence of one.
	Key:         KeyTraceRetentionDays,
	Kind:        KindInt,
	Group:       GroupTracing,
	Label:       "Trace retention (days)",
	Placeholder: "e.g. 30 — 0 keeps everything",
	Description: "Trace events older than this many days are pruned daily. 0 keeps everything; each generation span stores the full conversation it saw, so that grows fast.",
	Default:     "30",
	Min:         0,
}, {
	// Defaulted on purpose: the server always passes an explicit value, so
	// the SDK's OPENAI_AGENTS_TRACE_INCLUDE_SENSITIVE_DATA variable is not
	// consulted here — this switch is the one authority.
	Key:         KeyTraceIncludeSensitiveData,
	Kind:        KindBool,
	Group:       GroupTracing,
	Label:       "Trace sensitive data",
	Default:     "true",
	Description: "On records prompts, outputs and tool arguments in stored traces. Off leaves spans with only timing and usage metadata, and the trace panel's Replay nothing to seed from. Applies to new runs.",
}, {
	Key:         KeyTraceSpanDataKB,
	Kind:        KindInt,
	Group:       GroupTracing,
	Label:       "Stored span payload (KB)",
	Placeholder: "e.g. 8192",
	Description: "How much of a span's model request and response is stored. Past it the payload is replaced with a marker and a Replay of that call has nothing to seed from — raise it if you replay large turns. Live updates to the browser are capped separately at 256KB; what they drop is still in the trace. Applies to new runs.",
	Default:     "8192",
	Min:         1,
}, {
	Key:         KeyLogSensitiveData,
	Kind:        KindBool,
	Group:       GroupLogging,
	Label:       "Log sensitive data",
	Description: "Include prompts, tool arguments and model output in the SDK's own log records. Separate from the tracing switch: this one puts conversation content into stderr and whatever collects it. Off by default, and visible only at --log-level debug. Applies to new runs.",
	Default:     "false",
}, {
	Key:         KeyApprovalTTLMinutes,
	Kind:        KindInt,
	Group:       GroupLimits,
	Label:       "Approval timeout (minutes)",
	Placeholder: "e.g. 1440 — 0 disables expiry",
	Description: "How long a run may sit awaiting tool approval before it is expired and the wait is recorded in the transcript. 0 disables expiry.",
	Default:     "1440",
	Min:         0,
}, {
	Key:         KeyMaxTerminalsPerSandbox,
	Kind:        KindInt,
	Group:       GroupLimits,
	Label:       "Terminals per sandbox",
	Description: "Concurrent interactive terminals allowed on one sandbox — a fat-finger guard, not a scheduler.",
	Default:     "4",
	Min:         1,
	Max:         32,
}, {
	Key:         KeySandboxIdleMinutes,
	Kind:        KindInt,
	Group:       GroupLimits,
	Label:       "Sandbox idle stop (minutes)",
	Default:     "30",
	Min:         0,
	Description: "Stop a project's container after this many minutes with no run or terminal using it; 0 disables. The next run starts it again."}, {
	Key:         KeyPreviewEnabled,
	Kind:        KindBool,
	Group:       GroupLimits,
	Label:       "Port previews",
	Default:     "false",
	Description: "Let a project's owner reach a port inside its sandbox through this server. Off by default: it makes whatever is listening in there reachable by anyone who can sign in."}}

// Defs returns the registry in panel order.
func Defs() []Def { return defs }

// Lookup returns the def for key.
func Lookup(key string) (Def, bool) {
	for _, d := range defs {
		if d.Key == key {
			return d, true
		}
	}
	return Def{}, false
}

// IsSecret reports whether a key's value is masked on read and
// sentinel-resolved on write.
func IsSecret(key string) bool {
	d, ok := Lookup(key)
	return ok && d.Kind == KindSecret
}

// ErrUnknownKey is returned by Validate for a key the registry does not name.
var ErrUnknownKey = errors.New("unknown setting key")

// Validate reports whether value may be stored under key. An empty value is
// always allowed: it is how a setting is returned to its default.
func Validate(key, value string) error {
	d, ok := Lookup(key)
	if !ok {
		return fmt.Errorf("%q: %w", key, ErrUnknownKey)
	}
	v := strings.TrimSpace(value)
	if v == "" {
		return nil
	}
	switch d.Kind {
	case KindInt:
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("%s must be a whole number, got %q", key, value)
		}
		if n < d.Min {
			return fmt.Errorf("%s must be at least %d, got %d", key, d.Min, n)
		}
		if d.Max > 0 && n > d.Max {
			return fmt.Errorf("%s must be at most %d, got %d", key, d.Max, n)
		}
	case KindBool:
		if _, err := strconv.ParseBool(v); err != nil {
			return fmt.Errorf("%s must be true or false, got %q", key, value)
		}
	}
	if d.Validate != nil {
		return d.Validate(v)
	}
	return nil
}

// validateProxyURL rejects what Reader.ProxyClient would drop on the floor: an
// unparsable proxy, or one with no scheme to dial.
func validateProxyURL(v string) error {
	u, err := url.Parse(v)
	if err != nil {
		return fmt.Errorf("%s is not a URL: %w", KeyProxyURL, err)
	}
	if u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("%s needs a scheme and host, e.g. http://127.0.0.1:7890", KeyProxyURL)
	}
	return nil
}
