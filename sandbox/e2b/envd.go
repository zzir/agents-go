package e2b

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime/multipart"
	"net/http"
	"net/url"
	"path"
	"strings"

	"github.com/zzir/agents-go/sandbox"
)

// envd, the daemon inside every sandbox. Two surfaces: plain HTTP for file
// CONTENT (/files), Connect RPC for everything else.
const (
	procProcessStart = "/process.Process/Start"
	procSendInput    = "/process.Process/SendInput"
	procSendSignal   = "/process.Process/SendSignal"
	procUpdate       = "/process.Process/Update"
	procListDir      = "/filesystem.Filesystem/ListDir"
	procMakeDir      = "/filesystem.Filesystem/MakeDir"
	procMove         = "/filesystem.Filesystem/Move"
	procRemove       = "/filesystem.Filesystem/Remove"
	procStat         = "/filesystem.Filesystem/Stat"
)

// envdRequest builds a request to the sandbox's daemon, carrying whichever
// credential the configuration selects.
func (s *Sandbox) envdRequest(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	base, err := s.envdBase(ctx)
	if err != nil {
		return nil, err
	}
	return s.envdRequestAt(ctx, base, method, path, body)
}

// envdRequestAt is envdRequest against an explicit base, skipping envdBase's
// provisioning — for ensureWorkDir, which envdBase itself waits on.
func (s *Sandbox) envdRequestAt(ctx context.Context, base, method, path string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, base+path, body)
	if err != nil {
		return nil, fmt.Errorf("e2b: %s %s: %w", method, path, err)
	}
	s.authenticate(req)
	return req, nil
}

// authenticate applies the data-plane credential: E2B mints a per-sandbox
// token, a compatible service may take the API key — hence configuration.
func (s *Sandbox) authenticate(req *http.Request) {
	s.mu.Lock()
	token := s.accessToken
	s.mu.Unlock()
	switch s.opts.DataPlaneAuth {
	case AuthNone:
		return
	case AuthAPIKey:
		req.Header.Set("X-API-Key", s.opts.APIKey)
	case AuthAccessToken:
		req.Header.Set("X-Access-Token", token)
	default: // AuthAuto
		if token != "" {
			req.Header.Set("X-Access-Token", token)
			return
		}
		req.Header.Set("X-API-Key", s.opts.APIKey)
	}
}

/* ---------- files ---------- */

// ReadFile fetches a file's bytes over envd's /files endpoint.
func (s *Sandbox) ReadFile(ctx context.Context, p string) ([]byte, error) {
	req, err := s.envdRequest(ctx, http.MethodGet, s.filesPath(p), nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("e2b: read file %q: %w", p, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("e2b: read file %q: %w", p, fs.ErrNotExist)
	}
	if resp.StatusCode != http.StatusOK {
		payload, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, controlError(resp.StatusCode, payload)
	}
	return sandbox.ReadAllLimited(resp.Body, s.opts.MaxReadFileBytes)
}

// WriteFile uploads content, creating parent directories.
func (s *Sandbox) WriteFile(ctx context.Context, p string, content []byte) error {
	if dir := path.Dir(s.resolvePath(p)); dir != "." && dir != "/" {
		if err := s.makeDir(ctx, dir); err != nil {
			return err
		}
	}
	return s.upload(ctx, p, content)
}

// upload posts one file as multipart form data — the shape envd's /files
// endpoint takes.
func (s *Sandbox) upload(ctx context.Context, p string, content []byte) error {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, err := mw.CreateFormFile("file", path.Base(s.resolvePath(p)))
	if err != nil {
		return fmt.Errorf("e2b: write file %q: %w", p, err)
	}
	if _, err := part.Write(content); err != nil {
		return fmt.Errorf("e2b: write file %q: %w", p, err)
	}
	if err := mw.Close(); err != nil {
		return fmt.Errorf("e2b: write file %q: %w", p, err)
	}
	req, err := s.envdRequest(ctx, http.MethodPost, s.filesPath(p), &body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := s.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("e2b: write file %q: %w", p, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		payload, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return controlError(resp.StatusCode, payload)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

// filesPath builds the /files query for one path.
func (s *Sandbox) filesPath(p string) string {
	q := url.Values{}
	q.Set("path", s.resolvePath(p))
	q.Set("username", s.user())
	return "/files?" + q.Encode()
}

// CreateExclusive writes p only when it does not exist. envd has no atomic
// create, so the check happens INSIDE the sandbox: one shell command creates
// the file EMPTY under `set -C` — the shell's own noclobber, atomic against a
// concurrent tool call the way a check-then-upload from here would not be. The
// content then follows over /files (inlined in the argv it would hit Linux's
// ~128KB per-argument cap); an upload that fails takes the empty file with it,
// so a failed create never leaves a partial file behind.
func (s *Sandbox) CreateExclusive(ctx context.Context, p string, content []byte) error {
	full := s.resolvePath(p)
	script := "set -C; mkdir -p " + sandbox.ShellQuote(path.Dir(full)) +
		" && : > " + sandbox.ShellQuote(full)
	res, err := s.Exec(ctx, sandbox.ExecRequest{Cmd: []string{"sh", "-c", script}})
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		// set -C failed: stat the path rather than sniff a locale-dependent stderr —
		// found is the promised ErrExist, anything else is the real failure.
		if serr := s.unary(ctx, procStat, map[string]any{"path": full}, nil); serr == nil {
			return fmt.Errorf("e2b: create %q: %w", p, fs.ErrExist)
		}
		return fmt.Errorf("e2b: create %q: %s", p, strings.TrimSpace(res.Stderr))
	}
	if len(content) == 0 {
		return nil
	}
	if err := s.upload(ctx, p, content); err != nil {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), controlCallTimeout)
		defer cancel()
		_ = s.unary(rctx, procRemove, map[string]any{"path": full}, nil)
		return err
	}
	return nil
}

/* ---------- directory operations ---------- */

// entryInfo mirrors filesystem.EntryInfo in protojson camelCase; both scalars
// are loose because the compatible services render the SAME protobuf differently.
type entryInfo struct {
	Name string   `json:"name"`
	Type fileType `json:"type"`
	Path string   `json:"path"`
	// Size is an int64: protojson renders those as strings, but an older envd
	// renders them as numbers. json.Number takes either.
	Size json.Number `json:"size"`
}

// fileType decodes filesystem.FileType however the service spells it: E2B's
// envd 0.7 sends "FILE_TYPE_DIRECTORY", Alibaba Cloud's 0.5 the enum's NUMBER, 2.
type fileType struct{ dir bool }

func (f *fileType) UnmarshalJSON(b []byte) error {
	switch string(bytes.Trim(b, `"`)) {
	case "FILE_TYPE_DIRECTORY", "2", "dir", "directory":
		f.dir = true
	default:
		f.dir = false
	}
	return nil
}

func (e entryInfo) isDir() bool { return e.Type.dir }

func (e entryInfo) size() int64 {
	n, err := e.Size.Int64()
	if err != nil {
		return 0
	}
	return n
}

// ListDir lists one directory level.
func (s *Sandbox) ListDir(ctx context.Context, p string) ([]sandbox.DirEntry, error) {
	var out struct {
		Entries []entryInfo `json:"entries"`
	}
	err := s.unary(ctx, procListDir, map[string]any{"path": s.resolvePath(p), "depth": 1}, &out)
	if err != nil {
		if isNotFound(err) {
			return nil, fmt.Errorf("e2b: list dir %q: %w", p, fs.ErrNotExist)
		}
		return nil, err
	}
	entries := make([]sandbox.DirEntry, 0, len(out.Entries))
	for _, e := range out.Entries {
		entries = append(entries, sandbox.DirEntry{Name: e.Name, IsDir: e.isDir(), Size: e.size()})
	}
	return entries, nil
}

// RemoveFile deletes one path.
//
// envd's Remove is IDEMPOTENT: it answers OK for a path that was never there,
// so the absence has to be established first. Every other backend reports
// fs.ErrNotExist here, and apply_patch's rollback tells "deleted" from "was
// never there" by exactly that. The extra Stat is the price of the contract
// (verified against the real service — the suite caught this).
func (s *Sandbox) RemoveFile(ctx context.Context, p string) error {
	full := s.resolvePath(p)
	if err := s.unary(ctx, procStat, map[string]any{"path": full}, nil); err != nil {
		if isNotFound(err) {
			return fmt.Errorf("e2b: remove %q: %w", p, fs.ErrNotExist)
		}
		return err
	}
	err := s.unary(ctx, procRemove, map[string]any{"path": full}, nil)
	if isNotFound(err) {
		return fmt.Errorf("e2b: remove %q: %w", p, fs.ErrNotExist)
	}
	return err
}

// Rename moves a path, creating the destination's parents first — envd's Move
// does not.
func (s *Sandbox) Rename(ctx context.Context, oldPath, newPath string) error {
	dst := s.resolvePath(newPath)
	if dir := path.Dir(dst); dir != "." && dir != "/" {
		if err := s.makeDir(ctx, dir); err != nil {
			return err
		}
	}
	err := s.unary(ctx, procMove, map[string]any{"source": s.resolvePath(oldPath), "destination": dst}, nil)
	if isNotFound(err) {
		return fmt.Errorf("e2b: rename %q: %w", oldPath, fs.ErrNotExist)
	}
	return err
}

// makeDir creates a directory and its parents; already existing is success,
// matched on the CODE "already_exists" only, never on the message text.
func (s *Sandbox) makeDir(ctx context.Context, dir string) error {
	base, err := s.envdBase(ctx)
	if err != nil {
		return err
	}
	return s.makeDirAt(ctx, base, dir)
}

// makeDirAt is makeDir against an explicit base — see envdRequestAt.
func (s *Sandbox) makeDirAt(ctx context.Context, base, dir string) error {
	err := s.unaryAt(ctx, base, procMakeDir, map[string]any{"path": dir}, nil)
	if ce, ok := errors.AsType[*connectError](err); ok && ce.Code == "already_exists" {
		return nil
	}
	return err
}
