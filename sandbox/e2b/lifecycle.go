package e2b

import (
	"context"
	"fmt"
	"time"

	"github.com/zzir/agents-go/sandbox"
)

// Start provisions the sandbox — creating it, or resuming a paused one — and
// extends its lease. It is what ensure already does on the first command,
// exposed so a person can wait for the provisioning where they can see it.
func (s *Sandbox) Start(ctx context.Context) error {
	id, err := s.ensure(ctx)
	if err != nil {
		return err
	}
	if err := s.ensureWorkDir(ctx); err != nil {
		return err
	}
	return s.refresh(ctx, id, 0)
}

// Stop pauses the sandbox, keeping its filesystem. On a service that
// snapshots memory the processes come back too — which is MORE than
// sandbox.Lifecycle promises, and nothing here may rely on it (spec §2.7p).
//
// A sandbox that was never provisioned stops nothing: there is no compute to
// release, and creating one in order to pause it would be absurd.
func (s *Sandbox) Stop(ctx context.Context) error {
	s.mu.Lock()
	id := s.id
	s.mu.Unlock()
	if id == "" {
		return nil
	}
	err := s.pause(ctx, id)
	switch {
	case err == nil, isConflict(err):
		// Paused (or already paused — a 409, matched by status not message
		// text). Drop the lease so the next ensure takes the slow path and
		// resumes it, instead of the fast path handing back a paused sandbox.
		s.mu.Lock()
		s.leaseUntil = time.Time{}
		s.mu.Unlock()
		return nil
	case isNotFound(err):
		return nil
	}
	return err
}

// Status reports the sandbox's state without provisioning one: an id we have
// never had, or one the service no longer knows, is absent.
func (s *Sandbox) Status(ctx context.Context) (sandbox.State, error) {
	s.mu.Lock()
	id := s.id
	s.mu.Unlock()
	if id == "" {
		return sandbox.StateAbsent, nil
	}
	info, err := s.get(ctx, id)
	if err != nil {
		if isNotFound(err) {
			return sandbox.StateAbsent, nil
		}
		return sandbox.StateAbsent, err
	}
	if info.paused() {
		return sandbox.StateStopped, nil
	}
	return sandbox.StateRunning, nil
}

// URLForPort returns the public URL a service listening inside the sandbox is
// reachable at. The service builds one host per port, so this needs no call of
// its own — but it does need the sandbox to exist.
func (s *Sandbox) URLForPort(ctx context.Context, port int) (string, error) {
	if port <= 0 || port > 65535 {
		// A plain validation error: ErrLifecycleUnsupported would read as a
		// missing capability, which this service has.
		return "", fmt.Errorf("e2b: port %d is out of range", port)
	}
	id, err := s.ensure(ctx)
	if err != nil {
		return "", err
	}
	return "https://" + s.hostFor(id, port), nil
}

// Destroy kills the sandbox AND the stored state behind it. It is not part of
// any sandbox interface: destroying data is a decision the workbench makes on
// a project delete, never something a Close could do by accident.
func (s *Sandbox) Destroy(ctx context.Context) error {
	s.mu.Lock()
	id := s.id
	s.mu.Unlock()
	if id == "" {
		return nil
	}
	// Kill FIRST, forget only once it worked (already-gone counts): forgetting
	// on a failed kill would make a retry a no-op and leak the billed sandbox.
	if err := s.kill(ctx, id); err != nil && !isNotFound(err) {
		return err
	}
	s.mu.Lock()
	if s.id == id {
		s.forget()
	}
	s.mu.Unlock()
	return nil
}
