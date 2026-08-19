package settings

import (
	"context"
	"strconv"
	"strings"

	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// Reader reads typed setting values, falling back to each key's registered
// default. A nil Reader — or one built without a store — yields every default,
// so callers never branch on whether configuration is wired.
//
// Reads are not cached: every consumer reads per run, per connect or per tick,
// never per token, and a cache would owe an invalidation contract nobody asked
// for.
type Reader struct {
	store *store.SettingStore
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

// BoolPtr is Bool for a key whose default is "let the layer below decide":
// nil when nothing usable is stored and the registry names no default.
func (r *Reader) BoolPtr(ctx context.Context, key string) *bool {
	raw := r.resolve(ctx, key)
	if raw == "" {
		return nil
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		return nil
	}
	return &v
}
