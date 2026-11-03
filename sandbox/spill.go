package sandbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/zzir/agents-go/tracing"
)

// defaultSpillRetention is how long a spilled file is kept when SpillRetention is zero.
const defaultSpillRetention = 7 * 24 * time.Hour

// spillStream is one output stream and the extension its file takes.
type spillStream struct {
	ext string
	c   *captured
}

// spill saves every stream the model's view truncates under cfg.SpillDir in
// one exec and names the file in each stream's marker; a failure leaves the
// markers bare and goes on the span, never to the caller (spec §2.7v).
func (cfg CodeToolConfig) spill(ctx context.Context, sb Sandbox, span *tracing.SpanHandle, callID string, streams ...spillStream) {
	if cfg.SpillDir == "" {
		return
	}
	stamp := time.Now().UTC().Format("20060102T150405Z") + "-" + spillID(callID)
	files := make(map[string]string, len(streams))
	names := make([]string, len(streams))
	for i, s := range streams {
		if !s.c.truncated(cfg.MaxOutputBytes) {
			continue
		}
		names[i] = path.Join(cfg.SpillDir, stamp+s.ext)
		files[names[i]] = s.c.file()
	}
	if len(files) == 0 {
		return
	}
	res, err := sb.Exec(ctx, ExecRequest{
		Cmd:     []string{"sh", "-c", spillScript(cfg.SpillDir, cfg.SpillRetention)},
		Files:   files,
		Timeout: DefaultTimeout,
	})
	if err == nil && res.ExitCode != 0 {
		err = fmt.Errorf("exit %d: %s", res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	if err != nil {
		span.Set("spill_error", err.Error())
		return
	}
	saved := make([]string, 0, len(files))
	for i, s := range streams {
		if names[i] != "" {
			s.c.Saved, s.c.SavedHole = names[i], s.c.hole()
			saved = append(saved, names[i])
		}
	}
	span.Set("spill_path", saved)
}

// spillScript makes dir, prunes what is past retention and keeps git out of
// it; the files themselves arrive as ExecRequest.Files before it runs.
func spillScript(dir string, retention time.Duration) string {
	var b strings.Builder
	fmt.Fprintf(&b, "mkdir -p %s", ShellQuote(dir))
	if retention >= 0 {
		if retention == 0 {
			retention = defaultSpillRetention
		}
		// The files just written are 0 minutes old; +1 is the youngest cut that spares them.
		fmt.Fprintf(&b, " && find %s -type f ! -name .gitignore -mmin +%d -delete 2>/dev/null",
			ShellQuote(dir), max(1, int64(retention.Minutes())))
	}
	ignore := ShellQuote(dir + "/.gitignore")
	fmt.Fprintf(&b, "; [ -e %s ] || printf '*\\n' > %s", ignore, ignore)
	return b.String()
}

// spillID is the call id as a file name part — [A-Za-z0-9_-], at most 40
// bytes — or 8 random bytes without one.
func spillID(callID string) string {
	var b strings.Builder
	for _, r := range callID {
		if b.Len() == 40 {
			break
		}
		if r == '_' || r == '-' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	if b.Len() > 0 {
		return b.String()
	}
	var raw [8]byte
	_, _ = rand.Read(raw[:])
	return hex.EncodeToString(raw[:])
}
