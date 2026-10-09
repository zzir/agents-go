package e2b

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/zzir/agents-go/sandbox"
)

// Start provisions the sandbox — creating it, or resuming a paused one — on a
// fresh full lease; it is what ensure does on the first command, on request.
func (s *Sandbox) Start(ctx context.Context) error {
	if _, err := s.ensureFor(ctx, time.Duration(s.timeout())*time.Second); err != nil {
		return err
	}
	return s.ensureWorkDir(ctx)
}

// Stop pauses the sandbox, keeping its filesystem (spec §2.7p); a sandbox never
// provisioned stops nothing.
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
		// Paused (or already paused: a 409, matched by status). Drop the lease so the
		// next ensure takes the slow path and resumes it.
		s.mu.Lock()
		s.leaseUntil = time.Time{}
		s.mu.Unlock()
		return nil
	case isNotFound(err):
		return nil
	}
	return err
}

// Status reports the sandbox's state without provisioning one; a record that
// says running is confirmed through the daemon — see decisions §5.71.
func (s *Sandbox) Status(ctx context.Context) (sandbox.State, error) {
	id := s.currentID()
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
	// The daemon's credential rides on the record; the probe needs it.
	s.adopt(info)
	return s.health(ctx, id)
}

// Destroy kills the sandbox AND the stored state behind it; it is part of no
// sandbox interface, so no Close reaches it by accident.
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
	s.forget(id)
	return nil
}

// ErrNoSandbox is Address's answer when there is nothing to address: no
// sandbox provisioned yet, or the service no longer has the one this client held.
var ErrNoSandbox = errors.New("e2b: no sandbox to address")

// Address is where the sandbox's ports are public — "<port>-<id>.<domain>". It
// is a read: it neither provisions nor resumes (spec §2.7u).
func (s *Sandbox) Address(ctx context.Context) (id, domain string, err error) {
	id = s.currentID()
	if id == "" {
		return "", "", ErrNoSandbox
	}
	info, err := s.get(ctx, id)
	if err != nil {
		if isNotFound(err) {
			return "", "", fmt.Errorf("e2b: sandbox %s is gone: %w", id, ErrNoSandbox)
		}
		return "", "", err
	}
	if info.Domain != "" {
		return id, info.Domain, nil
	}
	return id, s.sandboxDomain(), nil
}
