package settings

import (
	"context"
	"strconv"
	"strings"

	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// Reader reads typed setting values, falling back to each key's registered
// default; a nil Reader, or one without a store, yields every default. Uncached.
type Reader struct {
	store *store.SettingStore
}

// NewReader returns a Reader over s. A nil store is valid and reads defaults.
func NewReader(s *store.SettingStore) *Reader { return &Reader{store: s} }

// raw returns the stored value with surrounding space removed, or "" when the
// setting is unset or unreadable.
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

// Int returns the stored number, or the key's default when unset or unparsable.
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

// SpanDataCap is the trace_span_data_kb setting in bytes: how much of one
// payload element the store keeps.
func (r *Reader) SpanDataCap(ctx context.Context) int {
	return r.Int(ctx, KeyTraceSpanDataKB) << 10
}

// SplitList parses a comma-separated flag or setting into trimmed, non-empty
// entries; stray spaces and trailing commas are dropped.
func SplitList(raw string) []string {
	var out []string
	for v := range strings.SplitSeq(raw, ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// S3Config is the attachment-storage section read as one value; Complete gates
// the feature, and a partial fill reads as off.
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
