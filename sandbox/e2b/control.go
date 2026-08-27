package e2b

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
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
// needed. It is the single gate every data-plane call passes through, and it
// holds the lock across the round trip so two concurrent commands cannot
// create two sandboxes for one client.
func (s *Sandbox) ensure(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.id != "" {
		info, err := s.get(ctx, s.id)
		switch {
		case err == nil && !info.paused():
			s.adopt(info)
			return s.id, nil
		case err == nil:
			resumed, rerr := s.resume(ctx, s.id)
			if rerr != nil {
				return "", rerr
			}
			s.adopt(resumed)
			return s.id, nil
		case isNotFound(err):
			// The sandbox is gone for good (killed, or expired without
			// auto-pause). Forget it and build a fresh one rather than
			// failing every command from here on.
			s.id, s.accessToken, s.domain = "", "", ""
		default:
			return "", err
		}
	}
	info, err := s.create(ctx)
	if err != nil {
		return "", err
	}
	id := info.id()
	if id == "" {
		return "", fmt.Errorf("e2b: the create returned no sandbox id")
	}
	// Record BEFORE adopting: a sandbox this client cannot hand back to its
	// owner is billed compute nobody will ever stop.
	if s.opts.OnSandboxID != nil {
		if err := s.opts.OnSandboxID(ctx, id); err != nil {
			return "", fmt.Errorf("e2b: recording sandbox %s: %w", id, err)
		}
	}
	s.id = id
	s.adopt(info)
	return s.id, nil
}

// adopt keeps the per-sandbox facts a response carried.
func (s *Sandbox) adopt(info sandboxInfo) {
	if info.EnvdAccessToken != "" {
		s.accessToken = info.EnvdAccessToken
	}
	if info.Domain != "" {
		s.domain = info.Domain
	}
}

// create provisions a new sandbox from the configured template.
func (s *Sandbox) create(ctx context.Context) (sandboxInfo, error) {
	body := map[string]any{
		"templateID": s.opts.TemplateID,
		"timeout":    s.timeout(),
	}
	if s.opts.AutoPause {
		body["autoPause"] = true
	}
	if s.opts.AllowInternet {
		body["allow_internet_access"] = true
	}
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
func (s *Sandbox) resume(ctx context.Context, id string) (sandboxInfo, error) {
	var out sandboxInfo
	err := s.control(ctx, http.MethodPost, "/sandboxes/"+id+"/connect", map[string]any{"timeout": s.timeout()}, &out)
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
func (s *Sandbox) refresh(ctx context.Context, id string) error {
	return s.control(ctx, http.MethodPost, "/sandboxes/"+id+"/timeout", map[string]any{"timeout": s.timeout()}, nil)
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
		// The message the service gave is what an operator needs: a quota, a
		// region, a template that is still building. Pass it through rather
		// than replacing it with a status line (protocol.md's rule for the
		// compatible services).
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
	if len(msg) > 300 {
		msg = msg[:300] + "…"
	}
	return fmt.Errorf("e2b: the service refused with %d: %s", status, msg)
}
