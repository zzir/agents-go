package settings

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"

	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// Reader reads typed setting values, falling back to each key's registered
// default. A nil Reader — or one built without a store — yields every default,
// so callers never branch on whether configuration is wired.
//
// Reads are not cached: every consumer reads per run, per connect or per tick,
// never per token, and a cache would owe an invalidation contract nobody asked
// for. ProxyClient pools the transport it BUILDS from a read, which is a
// different thing — the key is the value, so there is nothing to invalidate.
type Reader struct {
	store *store.SettingStore
	// transports pools one *http.Transport per proxy URL; see ProxyClient.
	transports sync.Map
}

// NewReader returns a Reader over s. A nil store is valid and reads defaults.
func NewReader(s *store.SettingStore) *Reader { return &Reader{store: s} }

// raw returns the stored value with surrounding space removed, or "" when the
// setting is unset or unreadable. Operators type these by hand, so a stray
// space must not turn a valid number into the default.
func (r *Reader) raw(ctx context.Context, key string) string {
	if r == nil || r.store == nil {
		return ""
	}
	s, err := r.store.Get(ctx, key)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(s.Value)
}

// resolve returns the stored value, or the registered default when unset.
func (r *Reader) resolve(ctx context.Context, key string) string {
	if v := r.raw(ctx, key); v != "" {
		return v
	}
	d, _ := Lookup(key)
	return d.Default
}

// String returns the stored value, or the key's default when unset.
func (r *Reader) String(ctx context.Context, key string) string {
	return r.resolve(ctx, key)
}

// Int returns the stored number. A value that no longer parses (stored before
// validation, or edited in the database) falls back to the default rather than
// taking the feature down.
func (r *Reader) Int(ctx context.Context, key string) int {
	n, err := strconv.Atoi(r.resolve(ctx, key))
	if err != nil {
		d, _ := Lookup(key)
		n, _ = strconv.Atoi(d.Default)
	}
	return n
}

// Bool returns the stored flag, or the key's default when unset or unparsable.
func (r *Reader) Bool(ctx context.Context, key string) bool {
	v, err := strconv.ParseBool(r.resolve(ctx, key))
	if err != nil {
		d, _ := Lookup(key)
		v, _ = strconv.ParseBool(d.Default)
	}
	return v
}

// ProxyClient returns a fresh *http.Client routed through the proxy_url
// setting, or nil when none is set. The client is the caller's to configure
// (its Timeout, say); what is shared is the transport behind it — one
// connection pool per proxy URL, keyed by the URL, so an edited setting lands
// on a new pool. A nil Reader reads no store, and proxy_url has no default,
// so it proxies nothing.
func (r *Reader) ProxyClient(ctx context.Context) *http.Client {
	u, err := url.Parse(r.String(ctx, KeyProxyURL))
	if r == nil || err != nil || u.String() == "" {
		return nil
	}
	key := u.String()
	t, ok := r.transports.Load(key)
	if !ok {
		t, _ = r.transports.LoadOrStore(key, &http.Transport{Proxy: http.ProxyURL(u)})
	}
	return &http.Client{Transport: t.(*http.Transport)}
}

// SpanDataCap is the trace_span_data_kb setting in bytes: how much of a span's
// payload the store keeps.
func (r *Reader) SpanDataCap(ctx context.Context) int {
	return r.Int(ctx, KeyTraceSpanDataKB) << 10
}

// SplitList parses a comma-separated flag or setting into trimmed, non-empty
// entries: operators type these by hand, so stray spaces and trailing commas
// are dropped rather than becoming names that match nothing.
func SplitList(raw string) []string {
	var out []string
	for v := range strings.SplitSeq(raw, ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// S3Config is the attachment-storage section read as one value. Complete()
// gates the feature: all-empty means image input is off, and a partial fill
// reads as off too (the settings handler refuses to store one, but rows
// predating a key's deletion must not half-configure the feature).
type S3Config struct {
	Endpoint      string
	Region        string
	Bucket        string
	AccessKeyID   string
	SecretKey     string
	PublicBaseURL string
	PathStyle     bool
}

// Complete reports whether every required field is set.
func (c S3Config) Complete() bool {
	return c.Endpoint != "" && c.Bucket != "" && c.AccessKeyID != "" && c.SecretKey != "" && c.PublicBaseURL != ""
}

// IsS3Key reports whether key belongs to the attachment-storage section.
func IsS3Key(key string) bool {
	switch key {
	case KeyS3Endpoint, KeyS3Region, KeyS3Bucket, KeyS3AccessKeyID,
		KeyS3SecretAccessKey, KeyS3PublicBaseURL, KeyS3PathStyle:
		return true
	}
	return false
}

// S3Config reads the attachment-storage settings.
func (r *Reader) S3Config(ctx context.Context) S3Config {
	return S3Config{
		Endpoint:      r.String(ctx, KeyS3Endpoint),
		Region:        r.String(ctx, KeyS3Region),
		Bucket:        r.String(ctx, KeyS3Bucket),
		AccessKeyID:   r.String(ctx, KeyS3AccessKeyID),
		SecretKey:     r.String(ctx, KeyS3SecretAccessKey),
		PublicBaseURL: r.String(ctx, KeyS3PublicBaseURL),
		PathStyle:     r.Bool(ctx, KeyS3PathStyle),
	}
}
