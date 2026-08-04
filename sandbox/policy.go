package sandbox

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Policy filters commands before they reach a human or a sandbox.
//
// It sits BEFORE approval, deliberately. An approval prompt asks a person to
// judge a command, and a person asked to judge forty commands an hour stops
// reading them — so the ones that were never going to be allowed should not
// reach the prompt at all, and the ones that are always fine should not either.
// Filtering after approval would be the opposite: it would ask, get a yes, and
// then refuse.
//
// The zero value allows everything, which keeps the sandbox's own position
// unchanged: it attaches no policy, the caller supplies one.
type Policy struct {
	// Allow, when non-empty, restricts execution to commands matching at least
	// one pattern. Anything else is refused.
	Allow []string
	// Deny refuses commands matching any pattern. It is checked AFTER Allow, so
	// a deny always wins — a policy that both allows `git .*` and denies
	// `git push` means what it looks like it means.
	Deny []string

	allow, deny []*regexp.Regexp
	compileErr  error
	compiled    bool
}

// Compile prepares the patterns, reporting a bad one.
//
// It is separate from evaluation so a malformed pattern fails when the policy
// is configured rather than on the first command it would have stopped —
// a policy that silently matched nothing would be worse than no policy, since
// it looks like protection.
func (p *Policy) Compile() error {
	if p.compiled {
		return p.compileErr
	}
	p.compiled = true
	var err error
	if p.allow, err = compilePatterns(p.Allow); err != nil {
		p.compileErr = fmt.Errorf("sandbox policy: allow: %w", err)
		return p.compileErr
	}
	if p.deny, err = compilePatterns(p.Deny); err != nil {
		p.compileErr = fmt.Errorf("sandbox policy: deny: %w", err)
		return p.compileErr
	}
	return nil
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

// Empty reports whether the policy filters nothing.
func (p *Policy) Empty() bool { return len(p.Allow) == 0 && len(p.Deny) == 0 }

// Check reports why a command is refused, or nil.
//
// The error names the pattern that refused it. A model told only "not allowed"
// tries variations; told which rule stopped it, it can ask for something else
// or explain to the user why it cannot proceed.
func (p *Policy) Check(cmd string) error {
	if p.Empty() {
		return nil
	}
	if err := p.Compile(); err != nil {
		// A policy that cannot be compiled refuses everything. Falling open
		// would turn a configuration typo into no protection at all, silently.
		return err
	}
	cmd = strings.TrimSpace(cmd)
	if len(p.allow) > 0 && !matchesAny(p.allow, cmd) {
		return &PolicyError{Command: cmd, Reason: "no allow pattern matches"}
	}
	for i, re := range p.deny {
		if re.MatchString(cmd) {
			return &PolicyError{Command: cmd, Reason: "denied by pattern " + strconv.Quote(p.Deny[i])}
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
