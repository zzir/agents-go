package e2b

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// The control plane: the six calls a sandbox's life needs. Everything else
// the API offers — templates, metrics, logs, forks, volumes — is deliberately
// absent (decisions §5.34).

// sandboxInfo is the service's view of one sandbox. Only the fields this
// package acts on are decoded; the rest of the response is ignored, so a
// service that returns more (or fewer) optional fields still works.
type sandboxInfo struct {
	SandboxID string `json:"sandboxID"`
	// Alternate spelling: not every compatible service emits E2B's exact
	// casing, and a create whose id we cannot read is a leaked sandbox.
	SandboxIDAlt    string `json:"sandbox_id"`
	Domain          string `json:"domain"`
	EnvdAccessToken string `json:"envdAccessToken"`
	State           string `json:"state"`
}

// id returns whichever spelling the service used.
func (i sandboxInfo) id() string {
	if i.SandboxID != "" {
		return i.SandboxID
	}
	return i.SandboxIDAlt
}

// paused reports the one state that needs a resume before use. An unknown or
// absent state reads as NOT paused: a needless resume is cheap, and treating
// "running" as paused would restart working sandboxes.
func (i sandboxInfo) paused() bool {
	return strings.EqualFold(i.State, "paused")
}

// ensure returns the sandbox id, creating or resuming the remote sandbox when
// needed. It is the single gate every data-plane call passes through.
func (s *Sandbox) ensure(ctx context.Context) (string, error) {
	return s.ensureFor(ctx, 0)
}

// ensureFor is ensure with a minimum lease runway: an operation bounded by
// runway (a tar export, a long exec) gets a lease that outlasts it. There is
// no keepalive, so an open-ended session can at best start from a full lease.
//
// Provisioning is serialized on provMu, so two concurrent commands cannot
// create two sandboxes for one client; s.mu is never held across a round trip
// or the OnSandboxID callback, so the callback may look at the sandbox.
func (s *Sandbox) ensureFor(ctx context.Context, runway time.Duration) (string, error) {
	// Fast path: a bound sandbox with a lease that is not near expiry needs no
	// control-plane call. If the sandbox died under us the data-plane call
	// fails once and the next ensure rebuilds.
	if id, ok := s.leased(runway); ok {
		return id, nil
	}
	s.provMu.Lock()
	defer s.provMu.Unlock()
	if id, ok := s.leased(runway); ok {
		return id, nil // provisioned while waiting for the lock
	}
	if id := s.currentID(); id != "" {
		info, err := s.get(ctx, id)
		switch {
		case err == nil && !info.paused():
			// Running: adopt its credential and extend the lease so a long
			// session does not lose the sandbox to its TTL.
			s.adopt(info)
			switch rerr := s.refresh(ctx, id, runway); {
			case rerr == nil:
				s.markLeased(runway)
				return id, nil
			case isNotFound(rerr):
				s.forget(id) // vanished between the read and the refresh; rebuild
			default:
				return "", rerr
			}
		case err == nil:
			resumed, rerr := s.resume(ctx, id, runway)
			if rerr != nil {
				return "", rerr
			}
			s.adopt(resumed)
			s.markLeased(runway)
			return id, nil
		case isNotFound(err):
			// Gone for good (killed, or expired without auto-pause): build a
			// fresh one rather than failing every command from here on.
			s.forget(id)
		default:
			return "", err
		}
	}
	info, err := s.create(ctx, runway)
	if err != nil {
		return "", err
	}
	id := info.id()
	if id == "" {
		return "", fmt.Errorf("e2b: the create returned no sandbox id")
	}
	// Bound before recorded, with the lease still unset: a concurrent command
	// waits on provMu rather than running on a sandbox the record may yet
	// reject, while the callback itself can inspect it (Status).
	s.mu.Lock()
	s.id = id
	s.freshWorkDir = true
	s.mu.Unlock()
	s.adopt(info)
	// A sandbox this client cannot hand back to its owner is billed compute
	// nobody will ever stop: a failed record kills it rather than leaking it.
	// The kill is bounded — a hung control plane must not wedge provMu forever.
	if s.opts.OnSandboxID != nil {
		if rerr := s.opts.OnSandboxID(ctx, id); rerr != nil {
			kctx, kcancel := context.WithTimeout(context.WithoutCancel(ctx), controlCallTimeout)
			defer kcancel()
			kerr := s.kill(kctx, id)
			s.forget(id)
			if kerr != nil {
				return "", fmt.Errorf("e2b: recording sandbox %s failed (%w) and killing it also failed: %w", id, rerr, kerr)
			}
			return "", fmt.Errorf("e2b: recording sandbox %s: %w", id, rerr)
		}
	}
	s.markLeased(runway)
	return id, nil
}

// leased returns the bound sandbox id when its lease has enough runway to
// skip a control-plane call.
func (s *Sandbox) leased(runway time.Duration) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.id, s.id != "" && s.leaseValid(runway)
}

// currentID returns the sandbox this client is bound to, or "".
func (s *Sandbox) currentID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.id
}

// adopt keeps the per-sandbox facts a response carried.
func (s *Sandbox) adopt(info sandboxInfo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if info.EnvdAccessToken != "" {
		s.accessToken = info.EnvdAccessToken
	}
	if info.Domain != "" {
		s.domain = info.Domain
	}
}

// create provisions a new sandbox from the configured template.
func (s *Sandbox) create(ctx context.Context, runway time.Duration) (sandboxInfo, error) {
	body := map[string]any{
		"templateID": s.opts.TemplateID,
		"timeout":    s.leaseSeconds(runway),
		// ALWAYS secure. Without it E2B's daemon takes no credential at all:
		// anyone who learns the sandbox id — which is in the public hostname
		// of every port a sandbox serves — can read its files and run
		// commands in it. Verified: a non-secure sandbox answers an
		// unauthenticated request 200, a secure one 401 (decisions §5.34).
		"secure": true,
	}
	if s.opts.AutoPause {
		body["autoPause"] = true
	}
	// Internet is OFF by default, matching the docker backend and the sandbox
	// package's isolation contract — so the field is sent either way, never
	// left to the service's own default (which is internet ON) — decisions §5.37.
	body["allow_internet_access"] = s.opts.AllowInternet
	if len(s.opts.Metadata) > 0 {
		body["metadata"] = s.opts.Metadata
	}
	if len(s.opts.Env) > 0 {
		body["envVars"] = s.opts.Env
	}
	var out sandboxInfo
	err := s.control(ctx, http.MethodPost, "/sandboxes", body, &out)
	return out, err
}

// get reads a sandbox's current state.
func (s *Sandbox) get(ctx context.Context, id string) (sandboxInfo, error) {
	var out sandboxInfo
	err := s.control(ctx, http.MethodGet, "/sandboxes/"+id, nil, &out)
	return out, err
}

// resume wakes a paused sandbox and extends its lease. `connect` rather than
// the deprecated `resume`: it is the endpoint both E2B and the compatible
// services document, and it does the resume when one is needed.
func (s *Sandbox) resume(ctx context.Context, id string, runway time.Duration) (sandboxInfo, error) {
	var out sandboxInfo
	err := s.control(ctx, http.MethodPost, "/sandboxes/"+id+"/connect", map[string]any{"timeout": s.leaseSeconds(runway)}, &out)
	return out, err
}

// pause suspends the sandbox, keeping its filesystem.
func (s *Sandbox) pause(ctx context.Context, id string) error {
	return s.control(ctx, http.MethodPost, "/sandboxes/"+id+"/pause", map[string]any{}, nil)
}

// kill destroys the sandbox AND its stored state.
func (s *Sandbox) kill(ctx context.Context, id string) error {
	return s.control(ctx, http.MethodDelete, "/sandboxes/"+id, nil, nil)
}

// refresh extends the sandbox's lease without touching anything else.
func (s *Sandbox) refresh(ctx context.Context, id string, runway time.Duration) error {
	return s.control(ctx, http.MethodPost, "/sandboxes/"+id+"/timeout", map[string]any{"timeout": s.leaseSeconds(runway)}, nil)
}

// control performs one control-plane call. A 404 comes back as a not_found
// connectError so the caller's isNotFound covers both planes.
func (s *Sandbox) control(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("e2b: encoding %s %s: %w", method, path, err)
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, s.apiURL()+path, body)
	if err != nil {
		return fmt.Errorf("e2b: %s %s: %w", method, path, err)
	}
	req.Header.Set("X-API-Key", s.opts.APIKey)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := s.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("e2b: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, maxFrameBytes))
	if err != nil {
		return fmt.Errorf("e2b: %s %s: %w", method, path, err)
	}
	if resp.StatusCode == http.StatusNotFound {
		return &connectError{Code: "not_found", Message: method + " " + path}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// The service's own message (a quota, a region, a template still
		// building) is what an operator needs; pass it through.
		return controlError(resp.StatusCode, payload)
	}
	if out == nil || len(payload) == 0 {
		return nil
	}
	if err := json.Unmarshal(payload, out); err != nil {
		return fmt.Errorf("e2b: decoding %s %s: %w", method, path, err)
	}
	return nil
}

// httpError is a non-2xx control-plane response, carrying the status so a
// caller can branch on it (a pause of an already-paused sandbox is a 409)
// rather than sniffing the message text.
type httpError struct {
	Status  int
	Message string
}

func (e *httpError) Error() string {
	return fmt.Sprintf("e2b: the service refused with %d: %s", e.Status, e.Message)
}

// isConflict reports a 409 from the control plane.
func isConflict(err error) bool {
	var he *httpError
	return errors.As(err, &he) && he.Status == http.StatusConflict
}

// maxErrBody caps an error message so a huge or HTML error page does not flood
// a log line or a tool result.
const maxErrBody = 300

// capBody caps s at maxErrBody, marking a truncation with an ellipsis.
func capBody(s string) string {
	if len(s) > maxErrBody {
		return s[:maxErrBody] + "…"
	}
	return s
}

// controlError extracts the service's own message from an error body.
func controlError(status int, payload []byte) error {
	var body struct {
		Message string `json:"message"`
		Error   string `json:"error"`
		Code    int    `json:"code"`
	}
	msg := ""
	if json.Unmarshal(payload, &body) == nil {
		msg = body.Message
		if msg == "" {
			msg = body.Error
		}
	}
	if msg == "" {
		msg = string(bytes.TrimSpace(payload))
	}
	return &httpError{Status: status, Message: capBody(msg)}
}
