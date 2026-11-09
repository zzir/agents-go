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

// The keys; every read, mask and validation names one of these, never a
// string literal.
const (
	KeySystemPrompt              = "system_prompt"
	KeyTraceRetentionDays        = "trace_retention_days"
	KeyTracePayloadRetentionDays = "trace_payload_retention_days"
	KeyTraceIncludeSensitiveData = "trace_include_sensitive_data"
	KeyTraceSpanDataKB           = "trace_span_data_kb"
	KeyLogSensitiveData          = "log_sensitive_data"
	KeyApprovalTTLMinutes        = "approval_ttl_minutes"
	KeyMaxTasksPerSession        = "max_tasks_per_session"
	KeyMaxTerminalsPerSandbox    = "max_terminals_per_sandbox"
	KeySandboxIdleMinutes        = "sandbox_idle_minutes"
	KeyS3Endpoint                = "s3_endpoint"
	KeyS3Region                  = "s3_region"
	KeyS3Bucket                  = "s3_bucket"
	KeyS3AccessKeyID             = "s3_access_key_id"
	KeyS3SecretAccessKey         = "s3_secret_access_key"
	KeyS3PublicBaseURL           = "s3_public_base_url"
	KeyS3PathStyle               = "s3_path_style"
	KeyTaskSessionRetentionDays  = "task_session_retention_days"
)

// The groups the panel renders as sections, in the order defs are listed. A
// group's defs sit together: the panel opens a section wherever the group changes.
const (
	GroupPrompt  = "prompt"
	GroupTracing = "tracing"
	GroupLogging = "logging"
	GroupLimits  = "limits"
	GroupStorage = "storage"
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

// defs is the table. Order is panel order: the prompt first (what every
// agent inherits), then the diagnostics and the caps.
var defs = []Def{{
	Key:         KeySystemPrompt,
	Kind:        KindText,
	Group:       GroupPrompt,
	Label:       "System prompt",
	Placeholder: "Optional instructions prepended to all agents",
	Description: "Prepended to every agent unless it overrides it. Keep it tool-agnostic; machine-specific instructions go in a sandbox's Prompt.",
}, {
	// Defaulted: "keep everything" is a choice, not the absence of one.
	Key:         KeyTraceRetentionDays,
	Kind:        KindInt,
	Group:       GroupTracing,
	Label:       "Trace retention (days)",
	Placeholder: "e.g. 30 — 0 keeps everything",
	Description: "Trace events older than this are pruned daily; a session left with none loses its payloads too. 0 keeps everything.",
	Default:     "30",
	Min:         0,
}, {
	Key:         KeyTracePayloadRetentionDays,
	Kind:        KindInt,
	Group:       GroupTracing,
	Label:       "Trace payload retention (days)",
	Placeholder: "e.g. 7 — unset keeps payloads as long as their spans",
	Description: "Days after its newest span that a session's request, reply and tool payloads are kept; spans stay. 0 keeps them with the spans.",
	Min:         0,
}, {
	// Passed explicitly as Observe.IncludeSensitiveData; the SDK reads no env
	// var (spec §2.14).
	Key:         KeyTraceIncludeSensitiveData,
	Kind:        KindBool,
	Group:       GroupTracing,
	Label:       "Trace sensitive data",
	Default:     "true",
	Description: "Record prompts, outputs and tool arguments in new runs' traces; off keeps timing and usage only.",
}, {
	Key:         KeyTraceSpanDataKB,
	Kind:        KindInt,
	Group:       GroupTracing,
	Label:       "Stored payload element (KB)",
	Placeholder: "e.g. 1024",
	Description: "Cap on one stored payload element (an item, tool arguments or result); past it that element becomes a marker.",
	Default:     "1024",
	Min:         1,
}, {
	Key:         KeyLogSensitiveData,
	Kind:        KindBool,
	Group:       GroupLogging,
	Label:       "Log sensitive data",
	Description: "Include prompts, tool arguments and model output in SDK log records, shown at --log-level debug.",
	Default:     "false",
}, {
	Key:         KeyApprovalTTLMinutes,
	Kind:        KindInt,
	Group:       GroupLimits,
	Label:       "Approval timeout (minutes)",
	Placeholder: "e.g. 1440 — 0 disables expiry",
	Description: "How long a run may await tool approval before it expires, the wait noted in the transcript. 0 disables expiry.",
	Default:     "1440",
	Min:         0,
}, {
	Key:         KeyMaxTasksPerSession,
	Kind:        KindInt,
	Group:       GroupLimits,
	Label:       "Background tasks per session",
	Description: "Live background tasks one session may run at once — a guard against runaway fan-out, read at each spawn.",
	Default:     "6", // mirrors tasks.DefaultMaxConcurrentPerParent
	Min:         1,
}, {
	Key:         KeyTaskSessionRetentionDays,
	Kind:        KindInt,
	Group:       GroupLimits,
	Label:       "Finished task transcripts (days)",
	Placeholder: "e.g. 30 — 0 keeps them forever",
	Description: "Days a finished background task keeps its transcript and row; its result stays in the session it reported to. 0 keeps them.",
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
	Description: "Stop a workspace's container after this many minutes with no run or terminal; the next run starts it again. 0 disables.",
}, {
	Key:         KeyS3Endpoint,
	Kind:        KindString,
	Group:       GroupStorage,
	Label:       "S3 endpoint",
	Placeholder: "https://<account>.r2.cloudflarestorage.com",
	Description: "S3-compatible API endpoint image attachments are uploaded to. Save probes the bucket first; Clear turns image input off.",
	Validate:    validateHTTPURL(KeyS3Endpoint),
}, {
	Key:         KeyS3Region,
	Kind:        KindString,
	Group:       GroupStorage,
	Label:       "S3 region",
	Placeholder: "auto",
	Default:     "auto",
	Description: "Signing region. R2 and MinIO accept \"auto\"; AWS needs the bucket's real region.",
}, {
	Key:         KeyS3Bucket,
	Kind:        KindString,
	Group:       GroupStorage,
	Label:       "S3 bucket",
	Description: "Bucket the images live in. It must allow PUBLIC READS — providers fetch them anonymously, as can anyone with a link.",
}, {
	Key:   KeyS3AccessKeyID,
	Kind:  KindString,
	Group: GroupStorage,
	Label: "S3 access key ID",
}, {
	Key:   KeyS3SecretAccessKey,
	Kind:  KindSecret,
	Group: GroupStorage,
	Label: "S3 secret access key",
}, {
	Key:         KeyS3PublicBaseURL,
	Kind:        KindString,
	Group:       GroupStorage,
	Label:       "Public base URL",
	Placeholder: "https://pub-….r2.dev or a CDN domain",
	Description: "Public prefix object keys are appended to. Changing it (or the bucket) without moving objects breaks past sessions' images.",
	Validate:    validateHTTPURL(KeyS3PublicBaseURL),
}, {
	Key:         KeyS3PathStyle,
	Kind:        KindBool,
	Group:       GroupStorage,
	Label:       "Path-style addressing",
	Default:     "false",
	Description: "On puts the bucket in the URL path (MinIO); off uses virtual-hosted addressing (AWS, R2).",
}}

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

// validateHTTPURL requires an absolute http(s) URL — for the endpoints the
// server (or a model provider) will dial.
func validateHTTPURL(key string) func(string) error {
	return func(v string) error {
		u, err := url.Parse(v)
		if err != nil {
			return fmt.Errorf("%s is not a URL: %w", key, err)
		}
		if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("%s must be an absolute http(s) URL", key)
		}
		return nil
	}
}
