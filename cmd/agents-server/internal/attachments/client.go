// Package attachments talks to the S3-compatible bucket image attachments
// live in: upload, delete, and the public URL an object is read back from.
// (The sentinel scheme entries store instead of that URL lives in store,
// beside the row it names.)
package attachments

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/zzir/agents-go/cmd/agents-server/internal/settings"
)

// The attachment limits, defined once and served to the client through the
// config endpoint.
const (
	// MaxBytes caps one uploaded image. The composer downscales first, so a
	// normal upload is far smaller; this bounds direct API callers.
	MaxBytes = 10 << 20
	// MaxPerMessage caps attachment_ids on one run request.
	MaxPerMessage = 8
	// MaxSidePx rejects absurd dimensions the byte cap alone would admit.
	MaxSidePx = 8000
	// DownscalePx is the longest-side target the composer resizes to before
	// uploading — served to the client, not enforced server-side.
	DownscalePx = 1568
)

// PublicURL joins the configured public base with an object key.
func PublicURL(base, key string) string {
	return strings.TrimRight(base, "/") + "/" + key
}

// Client performs the bucket operations. Built per call from the current
// settings (ClientFrom) — configuration is runtime-editable and the client
// holds no connection state of its own.
type Client struct {
	cfg  settings.S3Config
	http *http.Client
}

// ClientFrom returns a client for cfg, or nil when cfg is incomplete (the
// feature is off). httpClient nil uses http.DefaultClient; pass the proxy
// client so bucket traffic follows the proxy_url setting like all other
// outbound requests.
func ClientFrom(cfg settings.S3Config, httpClient *http.Client) *Client {
	if !cfg.Complete() {
		return nil
	}
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &Client{cfg: cfg, http: httpClient}
}

// objectURL is the S3-API URL for key: path-style puts the bucket in the
// path (MinIO), virtual-hosted prefixes it to the endpoint host (AWS, R2).
func (c *Client) objectURL(key string) string {
	ep := strings.TrimRight(c.cfg.Endpoint, "/")
	if c.cfg.PathStyle {
		return ep + "/" + c.cfg.Bucket + "/" + key
	}
	scheme, host, ok := strings.Cut(ep, "://")
	if !ok {
		return ep + "/" + c.cfg.Bucket + "/" + key // unreachable after validation
	}
	return scheme + "://" + c.cfg.Bucket + "." + host + "/" + key
}

// Put uploads data under key.
func (c *Client) Put(ctx context.Context, key, mime string, data []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.objectURL(key), bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("s3 put %s: %w", key, err)
	}
	req.ContentLength = int64(len(data))
	req.Header.Set("Content-Type", mime)
	signV4(req, c.cfg.AccessKeyID, c.cfg.SecretKey, c.cfg.Region, "s3", hexSHA256(data), time.Now())
	return c.do(req, "put", key)
}

// Delete removes key. A 404 is success: the reaper retries rows whose object
// may already be gone.
func (c *Client) Delete(ctx context.Context, key string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.objectURL(key), nil)
	if err != nil {
		return fmt.Errorf("s3 delete %s: %w", key, err)
	}
	signV4(req, c.cfg.AccessKeyID, c.cfg.SecretKey, c.cfg.Region, "s3", hexSHA256(nil), time.Now())
	return c.do(req, "delete", key)
}

// do executes a signed request and folds a non-2xx answer (404 excepted) into
// an error carrying the body's head — S3 errors are XML and the code inside
// is the part worth reading.
func (c *Client) do(req *http.Request, op, key string) error {
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("s3 %s %s: %w", op, key, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 || resp.StatusCode == http.StatusNotFound {
		return nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	return fmt.Errorf("s3 %s %s: %s: %s", op, key, resp.Status, strings.TrimSpace(string(body)))
}

// Probe verifies the configuration end to end: upload a marker object, fetch
// it back ANONYMOUSLY through the public base URL (the credential-less read
// is the point — model providers fetch that way), then delete it. Each stage
// failing names itself, so a bucket that accepts writes but is not publicly
// readable is caught at save time, not inside a run.
func (c *Client) Probe(ctx context.Context) error {
	key := "attachments/probe-" + fmt.Sprintf("%d", time.Now().UnixNano())
	body := []byte("agents-server attachment probe")
	if err := c.Put(ctx, key, "text/plain", body); err != nil {
		return fmt.Errorf("probe upload failed — check endpoint, bucket and keys: %w", err)
	}
	defer func() {
		delCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_ = c.Delete(delCtx, key)
	}()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, PublicURL(c.cfg.PublicBaseURL, key), nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("probe public read failed — check the public base URL: %w", err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(io.LimitReader(resp.Body, 128))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("probe public read got %s — the bucket must allow anonymous reads (model providers fetch attachment URLs without credentials)", resp.Status)
	}
	if !bytes.Equal(got, body) {
		return fmt.Errorf("probe public read returned different content — the public base URL does not serve this bucket")
	}
	return nil
}
