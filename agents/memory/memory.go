// Package memory gives a model memories of its own to read and write by
// scope: a conversation's working notes that survive compaction and a reset,
// and whatever else the host binds, an agent's memory say, under the host's
// own write rule (spec §2.5i).
package memory

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/zzir/agents-go/agents"
)

// Scope names one memory namespace: Kind says what it belongs to, ID which one.
type Scope struct {
	Kind string
	ID   string
}

// Info describes one memory without its content.
type Info struct {
	Key       string
	Bytes     int
	UpdatedAt time.Time
}

// ErrNotFound reports a key with no memory behind it.
var ErrNotFound = errors.New("memory: not found")

// Store keeps memories by scope and key. Append on a missing key creates it.
type Store interface {
	List(ctx context.Context, scope Scope) ([]Info, error)
	Read(ctx context.Context, scope Scope, key string) (string, error)
	Write(ctx context.Context, scope Scope, key, text string) error
	Append(ctx context.Context, scope Scope, key, text string) error
}

// Documented defaults (spec §4).
const (
	DefaultMaxBytes  = 1_000_000
	DefaultMaxKeys   = 100
	MaxKeyChars      = 200
	MaxQueryChars    = 1000
	MaxSearchFiles   = 20
	MaxSearchMatches = 10
	MaxReadChars     = 20_000
	MaxMatchChars    = 500
)

// ScopeSpec is one scope the tools expose, under the name the model passes.
type ScopeSpec struct {
	// Scope is the concrete scope, when the host knows it at build time; a
	// Resolver may fill it per call instead.
	Scope Scope
	// Name is what the model passes as scope.
	Name string
	// Writable admits memory_write and memory_append; a read-only scope
	// answers a write with a refusal the model can read.
	Writable bool
	// Approve pauses a write for approval (spec §2.7).
	Approve bool
	// MaxBytes caps one key's content; 0 means DefaultMaxBytes. MaxKeys caps
	// the keys a scope holds; 0 means DefaultMaxKeys.
	MaxBytes int
	MaxKeys  int
	// Describe says what the scope is for, one line in the tool descriptions.
	Describe string
}

// Resolver opens the store and concrete scope a call works on. An error
// reaches the model as the tool's failure.
type Resolver func(ctx context.Context, rc *agents.RunContext, spec ScopeSpec) (Store, Scope, error)

// Static resolves every spec to store and its declared Scope.
func Static(store Store) Resolver {
	return func(_ context.Context, _ *agents.RunContext, spec ScopeSpec) (Store, Scope, error) {
		return store, spec.Scope, nil
	}
}

type listArgs struct {
	Scope string `json:"scope" jsonschema:"Which memory: a scope name from the tool description; empty means the first"`
}

type readArgs struct {
	Scope     string `json:"scope" jsonschema:"Which memory: a scope name from the tool description; empty means the first"`
	Key       string `json:"key" jsonschema:"The memory's key, as memory_list shows it"`
	StartLine int    `json:"start_line" jsonschema:"First line to return, 1-based; 0 means the beginning; negative counts back from the last line"`
	EndLine   int    `json:"end_line" jsonschema:"Last line to return, inclusive; 0 means the end; negative counts back from the last line"`
	Offset    int    `json:"offset" jsonschema:"Character offset into the selected lines to start at; 0 for their beginning"`
	Limit     int    `json:"limit" jsonschema:"Characters to return, at most 20000; 0 means 20000"`
}

type searchArgs struct {
	Scope             string `json:"scope" jsonschema:"Which memory: a scope name from the tool description; empty means the first"`
	Query             string `json:"query" jsonschema:"Case-insensitive literal text to find in memory lines"`
	KeyPrefix         string `json:"key_prefix" jsonschema:"Only keys starting with this; empty searches every key"`
	MaxFiles          int    `json:"max_files" jsonschema:"Keys to report at most, up to 20; 0 means 20"`
	MaxMatchesPerFile int    `json:"max_matches_per_file" jsonschema:"Matching lines per key at most, up to 10; 0 means 10"`
}

type writeArgs struct {
	Scope string `json:"scope" jsonschema:"Which memory: a scope name from the tool description; empty means the first"`
	Key   string `json:"key" jsonschema:"A path-like key such as plan.md or findings/auth.md: no empty segments, no . or .., at most 200 characters"`
	Text  string `json:"text" jsonschema:"The text to store"`
}

// Tools returns memory_list, memory_read, memory_search, memory_write and
// memory_append over the scopes, the first being the default. The scope
// rules the model reads are built from scopes here; resolve opens each call.
func Tools(scopes []ScopeSpec, resolve Resolver) []*agents.Tool {
	if len(scopes) == 0 {
		panic("memory.Tools: at least one scope is required")
	}
	t := &tools{scopes: scopes, resolve: resolve}
	intro := t.intro()
	list := agents.NewTool("memory_list",
		"List the keys in one of your memories with their sizes. "+intro,
		t.list)
	list.ReadOnly = true
	read := agents.NewTool("memory_read",
		"Read one memory by key: whole, a line range, or a character window (offset, limit) over the selected lines, at most 20000 characters per call. "+intro,
		t.read)
	read.ReadOnly = true
	search := agents.NewTool("memory_search",
		"Find the lines of your memories that contain a text, case-insensitively. "+intro,
		t.search)
	search.ReadOnly = true
	write := agents.NewTool("memory_write",
		"Create or replace one memory. "+intro,
		t.write)
	write.NeedsApproval = true
	write.NeedsApprovalFunc = t.needsApproval
	appendT := agents.NewTool("memory_append",
		"Add text to the end of one memory, creating it when new. "+intro,
		t.append)
	appendT.NeedsApproval = true
	appendT.NeedsApprovalFunc = t.needsApproval
	return []*agents.Tool{list, read, search, write, appendT}
}

type tools struct {
	scopes  []ScopeSpec
	resolve Resolver
}

// intro is the scope table every description ends with.
func (t *tools) intro() string {
	var b strings.Builder
	b.WriteString("Scopes:")
	for i, s := range t.scopes {
		fmt.Fprintf(&b, " %s", s.Name)
		if i == 0 {
			b.WriteString(" (default)")
		}
		b.WriteString(": ")
		b.WriteString(s.Describe)
		switch {
		case !s.Writable:
			b.WriteString(" Read-only.")
		case s.Approve:
			b.WriteString(" A write waits for the user's approval.")
		}
	}
	b.WriteString(" The user sees these memories too.")
	return b.String()
}

func (t *tools) spec(name string) (ScopeSpec, bool) {
	if name == "" {
		return t.scopes[0], true
	}
	for _, s := range t.scopes {
		if s.Name == name {
			return s, true
		}
	}
	return ScopeSpec{}, false
}

func (t *tools) unknownScope(name string) string {
	names := make([]string, len(t.scopes))
	for i, s := range t.scopes {
		names[i] = s.Name
	}
	return fmt.Sprintf("No memory scope %q; the scopes are %s.", name, strings.Join(names, ", "))
}

// open resolves the call's scope; a text answer means the model chose badly.
func (t *tools) open(ctx context.Context, rc *agents.RunContext, name string) (Store, Scope, ScopeSpec, string, error) {
	spec, ok := t.spec(name)
	if !ok {
		return nil, Scope{}, spec, t.unknownScope(name), nil
	}
	store, scope, err := t.resolve(ctx, rc, spec)
	if err != nil {
		return nil, Scope{}, spec, "", err
	}
	return store, scope, spec, "", nil
}

// needsApproval reads the scope off the arguments: undecodable arguments
// need nobody, since the call then fails on the model and writes nothing.
func (t *tools) needsApproval(_ context.Context, _ *agents.RunContext, argsJSON, _ string) (bool, error) {
	var a struct {
		Scope string `json:"scope"`
	}
	if !decodeArgs(argsJSON, &a) {
		return false, nil
	}
	spec, ok := t.spec(a.Scope)
	return ok && spec.Writable && spec.Approve, nil
}

func (t *tools) list(ctx context.Context, tc *agents.ToolContext, a listArgs) (string, error) {
	store, scope, spec, msg, err := t.open(ctx, tc.RunContext, a.Scope)
	if err != nil || msg != "" {
		return msg, err
	}
	infos, err := store.List(ctx, scope)
	if err != nil {
		return "", err
	}
	if len(infos) == 0 {
		return fmt.Sprintf("No memories in %s yet.", spec.Name), nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d memories in %s:\n", len(infos), spec.Name)
	for _, in := range infos {
		fmt.Fprintf(&b, "- %s (%d bytes", in.Key, in.Bytes)
		if !in.UpdatedAt.IsZero() {
			fmt.Fprintf(&b, ", updated %s", in.UpdatedAt.UTC().Format(time.RFC3339))
		}
		b.WriteString(")\n")
	}
	return b.String(), nil
}

func (t *tools) read(ctx context.Context, tc *agents.ToolContext, a readArgs) (string, error) {
	store, scope, spec, msg, err := t.open(ctx, tc.RunContext, a.Scope)
	if err != nil || msg != "" {
		return msg, err
	}
	if a.Key == "" {
		return "Pass the key of the memory to read; memory_list shows them.", nil
	}
	text, err := store.Read(ctx, scope, a.Key)
	if errors.Is(err, ErrNotFound) {
		return fmt.Sprintf("No memory %q in %s.", a.Key, spec.Name), nil
	}
	if err != nil {
		return "", err
	}
	lines := strings.Split(text, "\n")
	start, end := lineRange(a.StartLine, a.EndLine, len(lines))
	if start > end {
		return fmt.Sprintf("%s has %d lines; the range selects none.", a.Key, len(lines)), nil
	}
	selected := []rune(strings.Join(lines[start-1:end], "\n"))
	offset := min(max(a.Offset, 0), len(selected))
	limit := a.Limit
	if limit <= 0 || limit > MaxReadChars {
		limit = MaxReadChars
	}
	stop := min(offset+limit, len(selected))
	var b strings.Builder
	if start != 1 || end != len(lines) {
		fmt.Fprintf(&b, "Lines %d-%d of %d:\n", start, end, len(lines))
	}
	b.WriteString(string(selected[offset:stop]))
	if stop < len(selected) {
		fmt.Fprintf(&b, "\n(%d more characters after offset %d; pass offset=%d to continue)", len(selected)-stop, stop, stop)
	}
	return b.String(), nil
}

// lineRange resolves a 1-based inclusive range over n lines: 0 means the
// edge, a negative value counts back from the last line.
func lineRange(startLine, endLine, n int) (start, end int) {
	resolve := func(v, edge int) int {
		switch {
		case v == 0:
			return edge
		case v < 0:
			return n + v + 1
		}
		return v
	}
	start = max(resolve(startLine, 1), 1)
	end = min(resolve(endLine, n), n)
	return start, end
}

func (t *tools) search(ctx context.Context, tc *agents.ToolContext, a searchArgs) (string, error) {
	store, scope, spec, msg, err := t.open(ctx, tc.RunContext, a.Scope)
	if err != nil || msg != "" {
		return msg, err
	}
	if a.Query == "" {
		return "Pass the text to search for.", nil
	}
	if len([]rune(a.Query)) > MaxQueryChars {
		return fmt.Sprintf("The query is longer than %d characters; search for a shorter piece of it.", MaxQueryChars), nil
	}
	maxFiles := a.MaxFiles
	if maxFiles <= 0 || maxFiles > MaxSearchFiles {
		maxFiles = MaxSearchFiles
	}
	maxMatches := a.MaxMatchesPerFile
	if maxMatches <= 0 || maxMatches > MaxSearchMatches {
		maxMatches = MaxSearchMatches
	}
	infos, err := store.List(ctx, scope)
	if err != nil {
		return "", err
	}
	needle := strings.ToLower(a.Query)
	var b strings.Builder
	files := 0
	for _, in := range infos {
		if a.KeyPrefix != "" && !strings.HasPrefix(in.Key, a.KeyPrefix) {
			continue
		}
		text, err := store.Read(ctx, scope, in.Key)
		if err != nil {
			return "", err
		}
		var matches []string
		for i, line := range strings.Split(text, "\n") {
			if strings.Contains(strings.ToLower(line), needle) {
				matches = append(matches, fmt.Sprintf("  %d: %s", i+1, clipRunes(line, MaxMatchChars)))
				if len(matches) == maxMatches {
					break
				}
			}
		}
		if len(matches) == 0 {
			continue
		}
		if files == maxFiles {
			b.WriteString("More keys match; narrow the query or key_prefix.\n")
			break
		}
		files++
		fmt.Fprintf(&b, "%s:\n%s\n", in.Key, strings.Join(matches, "\n"))
	}
	if files == 0 {
		return fmt.Sprintf("Nothing in %s contains %q.", spec.Name, a.Query), nil
	}
	return b.String(), nil
}

func (t *tools) write(ctx context.Context, tc *agents.ToolContext, a writeArgs) (string, error) {
	return t.store(ctx, tc, a, false)
}

func (t *tools) append(ctx context.Context, tc *agents.ToolContext, a writeArgs) (string, error) {
	return t.store(ctx, tc, a, true)
}

// store is the write path both mutating tools share: the scope must be
// writable, the key valid, and the result within the scope's limits.
func (t *tools) store(ctx context.Context, tc *agents.ToolContext, a writeArgs, appendTo bool) (string, error) {
	store, scope, spec, msg, err := t.open(ctx, tc.RunContext, a.Scope)
	if err != nil || msg != "" {
		return msg, err
	}
	if !spec.Writable {
		return fmt.Sprintf("%s is read-only for you; %s", spec.Name, spec.Describe), nil
	}
	if msg := keyProblem(a.Key); msg != "" {
		return msg, nil
	}
	maxBytes := spec.MaxBytes
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}
	maxKeys := spec.MaxKeys
	if maxKeys <= 0 {
		maxKeys = DefaultMaxKeys
	}
	infos, err := store.List(ctx, scope)
	if err != nil {
		return "", err
	}
	size, exists := 0, false
	for _, in := range infos {
		if in.Key == a.Key {
			size, exists = in.Bytes, true
			break
		}
	}
	if !exists && len(infos) >= maxKeys {
		return fmt.Sprintf("%s already holds %d memories, its limit; replace or reuse one.", spec.Name, maxKeys), nil
	}
	total := len(a.Text)
	if appendTo {
		total += size
	}
	if total > maxBytes {
		return fmt.Sprintf("That would make %q %d bytes; %s allows %d per memory. Split it across keys or shorten it.", a.Key, total, spec.Name, maxBytes), nil
	}
	if appendTo {
		err = store.Append(ctx, scope, a.Key, a.Text)
	} else {
		err = store.Write(ctx, scope, a.Key, a.Text)
	}
	if err != nil {
		return "", err
	}
	verb := "Wrote"
	if appendTo {
		verb = "Appended to"
	}
	return fmt.Sprintf("%s %s in %s (%d bytes now).", verb, a.Key, spec.Name, total), nil
}

// keyProblem is ValidKey's verdict as the text a model reads, "" when fine.
func keyProblem(key string) string {
	if err := ValidKey(key); err != nil {
		return err.Error()
	}
	return ""
}

// ValidKey checks a memory key: path-like, no empty segments, no . or ..,
// printable, at most MaxKeyChars characters.
func ValidKey(key string) error {
	if key == "" {
		return errors.New("a memory key is required")
	}
	if len([]rune(key)) > MaxKeyChars {
		return fmt.Errorf("the key %q is longer than %d characters", key, MaxKeyChars)
	}
	for _, r := range key {
		if unicode.IsControl(r) {
			return fmt.Errorf("the key %q contains a control character", key)
		}
	}
	if strings.HasPrefix(key, "/") {
		return fmt.Errorf("the key %q must be relative", key)
	}
	for _, seg := range strings.Split(key, "/") {
		switch strings.TrimSpace(seg) {
		case "", ".", "..":
			return fmt.Errorf("the key %q has an empty, . or .. segment", key)
		}
	}
	return nil
}

// Snapshot renders a scope's memories as one text of at most maxChars bytes
// (0 for no bound): every memory in full while they fit, then the rest as a
// list of keys and sizes, itself cut to what fits. Empty when the scope holds
// nothing.
func Snapshot(ctx context.Context, store Store, scope Scope, maxChars int) (string, error) {
	infos, err := store.List(ctx, scope)
	if err != nil {
		return "", err
	}
	if len(infos) == 0 {
		return "", nil
	}
	sort.Slice(infos, func(i, j int) bool { return infos[i].Key < infos[j].Key })
	fits := func(b *strings.Builder, more string) bool { return maxChars <= 0 || b.Len()+len(more) <= maxChars }
	var b strings.Builder
	var rest []Info
	for i, in := range infos {
		text, err := store.Read(ctx, scope, in.Key)
		if err != nil {
			return "", err
		}
		section := fmt.Sprintf("## %s\n%s\n\n", in.Key, text)
		if !fits(&b, section) {
			rest = infos[i:]
			break
		}
		b.WriteString(section)
	}
	if len(rest) > 0 {
		b.WriteString("## Not shown (read with memory_read)\n")
		for i, in := range rest {
			line := fmt.Sprintf("- %s (%d bytes)\n", in.Key, in.Bytes)
			tail := fmt.Sprintf("- and %d more\n", len(rest)-i)
			// The last line needs no room left for a tail after it.
			need := line
			if i < len(rest)-1 {
				need += tail
			}
			if !fits(&b, need) {
				b.WriteString(tail)
				break
			}
			b.WriteString(line)
		}
	}
	return capBytes(strings.TrimRight(b.String(), "\n"), maxChars), nil
}

// clipRunes cuts a line to n runes, marking the cut.
func clipRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + fmt.Sprintf("… (%d more characters; memory_read shows the rest)", len(r)-n)
}

// capBytes cuts s to at most n bytes on a rune boundary; n <= 0 leaves it.
func capBytes(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}
