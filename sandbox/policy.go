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
// It is a filter on approval noise, NOT a security boundary. A pattern matches
// the TEXT of a command, and a shell spells one command in unbounded ways.
// `Deny: []string{"rm -rf"}` stops `rm -rf /`, and steps aside for `rm -fr /`,
// for `rm  -rf /` with a second space, and for the base64 in
// `eval $(echo cm0gLXJm | base64 -d)`, which is not the command until bash
// expands it. Naming a path fares no better — a rule denying
// `rm -rf /home/alice` never sees `rm -rf $HOME`. Containment comes from the
// sandbox a command executes in: choose a backend whose isolation you trust,
// and treat the policy as what it is, a way to keep the obvious out of a
// person's face.
//
// The zero value allows everything, which keeps the sandbox's own position
// unchanged: it attaches no policy, the caller supplies one.
//
// A Policy is plain configuration and is copied freely; the compiled patterns
// live outside it, so no two copies share mutable state.
type Policy struct {
	// Allow, when non-empty, restricts execution to commands matching at least
	// one pattern. Anything else is refused.
	Allow []string
	// Deny refuses commands matching any pattern. It is checked AFTER Allow, so
	// a deny always wins — a policy that both allows `git .*` and denies
	// `git push` means what it looks like it means.
	Deny []string
}

// Compile reports a malformed pattern. It validates and nothing more: the
// compiled form is not kept.
//
// It is separate from evaluation so a malformed pattern fails when the policy
// is configured rather than on the first command it would have stopped —
// a policy that silently matched nothing would be worse than no policy, since
// it looks like protection.
func (p *Policy) Compile() error {
	_, err := p.compile()
	return err
}

// Empty reports whether the policy filters nothing.
func (p *Policy) Empty() bool { return len(p.Allow) == 0 && len(p.Deny) == 0 }

// Check reports why a command is refused, or nil.
//
// The error names the pattern that refused it. A model told only "not allowed"
// tries variations; told which rule stopped it, it can ask for something else
// or explain to the user why it cannot proceed.
//
// The patterns are compiled per call — nothing next to the cost of executing a
// command, and the price of a Policy that is an inert value rather than a cache
// several goroutines take turns filling. CodeTool, inside the package, keeps a
// compiled form for the tool's lifetime.
func (p *Policy) Check(cmd string) error {
	if p.Empty() {
		return nil
	}
	compiled, err := p.compile()
	if err != nil {
		// A policy that cannot be compiled refuses everything. Falling open
		// would turn a configuration typo into no protection at all, silently.
		return err
	}
	return compiled.check(cmd)
}

// compiledPolicy is a policy's patterns, compiled. Nothing writes to one after
// compile returns it, which is what makes it safe to share between the tool
// calls the runner executes in parallel.
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
			// re.String is the pattern as it was written, so the reason names
			// the rule the caller configured.
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
