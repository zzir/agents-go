package sandbox

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Policy filters commands before they reach a human or a sandbox. It runs
// BEFORE the approval gate and is a filter on approval noise, not a security
// boundary — see spec §2.7j. The zero value allows everything.
//
// A Policy is plain configuration and is copied freely; the compiled patterns
// live outside it, so no two copies share mutable state.
type Policy struct {
	// Allow, when non-empty, restricts execution to commands matching at least
	// one pattern. Anything else is refused.
	Allow []string
	// Deny refuses commands matching any pattern. It is checked AFTER Allow, so
	// a deny always wins.
	Deny []string
}

// Compile reports a malformed pattern without keeping the compiled form, so a
// policy fails when it is configured rather than on the first command it
// would have stopped.
func (p *Policy) Compile() error {
	_, err := p.compile()
	return err
}

// Empty reports whether the policy filters nothing.
func (p *Policy) Empty() bool { return len(p.Allow) == 0 && len(p.Deny) == 0 }

// Check reports why a command is refused, or nil. The error names the pattern
// that refused it (spec §2.7j). Patterns are compiled per call; CodeTool keeps
// a compiled form for the tool's lifetime.
func (p *Policy) Check(cmd string) error {
	if p.Empty() {
		return nil
	}
	compiled, err := p.compile()
	if err != nil {
		// A policy that cannot be compiled refuses everything (spec §2.7j).
		return err
	}
	return compiled.check(cmd)
}

// compiledPolicy is a policy's patterns, compiled. Nothing writes to one after
// compile returns it, so parallel tool calls may share it.
type compiledPolicy struct {
	allow, deny []*regexp.Regexp
}

func (p *Policy) compile() (*compiledPolicy, error) {
	allow, err := compilePatterns(p.Allow)
	if err != nil {
		return nil, fmt.Errorf("sandbox policy: allow: %w", err)
	}
	deny, err := compilePatterns(p.Deny)
	if err != nil {
		return nil, fmt.Errorf("sandbox policy: deny: %w", err)
	}
	return &compiledPolicy{allow: allow, deny: deny}, nil
}

func compilePatterns(pats []string) ([]*regexp.Regexp, error) {
	out := make([]*regexp.Regexp, 0, len(pats))
	for _, pat := range pats {
		re, err := regexp.Compile(pat)
		if err != nil {
			return nil, fmt.Errorf("pattern %q: %w", pat, err)
		}
		out = append(out, re)
	}
	return out, nil
}

func (c *compiledPolicy) check(cmd string) error {
	cmd = strings.TrimSpace(cmd)
	if len(c.allow) > 0 && !matchesAny(c.allow, cmd) {
		return &PolicyError{Command: cmd, Reason: "no allow pattern matches"}
	}
	for _, re := range c.deny {
		if re.MatchString(cmd) {
			// re.String is the pattern as written, so the reason names the
			// rule the caller configured.
			return &PolicyError{Command: cmd, Reason: "denied by pattern " + strconv.Quote(re.String())}
		}
	}
	return nil
}

func matchesAny(res []*regexp.Regexp, s string) bool {
	for _, re := range res {
		if re.MatchString(s) {
			return true
		}
	}
	return false
}

// PolicyError reports a command the policy refused.
type PolicyError struct {
	Command string
	Reason  string
}

func (e *PolicyError) Error() string {
	return fmt.Sprintf("command refused by policy (%s): %s", e.Reason, e.Command)
}
