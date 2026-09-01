package store

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestSettingModify(t *testing.T) {
	ctx := context.Background()
	withTestBox(t)
	st := NewSettingStore(newTestDB(t))
	st.SealIf(func(key string) bool { return key == "secret_key" })

	// Nothing stored: found is false, and the answer is inserted.
	err := st.Modify(ctx, "secret_key", func(prev string, found bool) (string, error) {
		if found || prev != "" {
			t.Errorf("first Modify saw prev=%q found=%v", prev, found)
		}
		return "s1", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// Stored: the opened value is handed over, and the answer replaces it.
	err = st.Modify(ctx, "secret_key", func(prev string, found bool) (string, error) {
		if !found || prev != "s1" {
			t.Errorf("second Modify saw prev=%q found=%v", prev, found)
		}
		return prev + "-kept", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := st.Get(ctx, "secret_key"); got.Value != "s1-kept" {
		t.Fatalf("value = %q", got.Value)
	}
	// value's error comes back as given and nothing is written.
	refused := errors.New("refused")
	err = st.Modify(ctx, "secret_key", func(string, bool) (string, error) { return "never", refused })
	if !errors.Is(err, refused) {
		t.Fatalf("Modify = %v, want the callback's error", err)
	}
	if got, _ := st.Get(ctx, "secret_key"); got.Value != "s1-kept" {
		t.Fatalf("a refused Modify wrote: value = %q", got.Value)
	}
}

// The storage form's group write seals its secret exactly as the per-key
// path does: the raw column holds ciphertext, reads come back plaintext, and
// an empty value deletes the row.
func TestSetManySealsSecrets(t *testing.T) {
	withTestBox(t)
	db := newTestDB(t)
	s := NewSettingStore(db)
	s.SealIf(func(key string) bool { return key == "s3_secret_access_key" })
	ctx := context.Background()

	if err := s.SetMany(ctx, map[string]string{
		"s3_secret_access_key": "super-secret",
		"s3_bucket":            "imgs",
	}); err != nil {
		t.Fatal(err)
	}

	raw := rawColumn(t, db, "SELECT value FROM settings WHERE key = ?", "s3_secret_access_key")
	if raw == "super-secret" || !strings.Contains(raw, "enc:") {
		t.Fatalf("secret stored as %q — not sealed", raw)
	}
	if got := rawColumn(t, db, "SELECT value FROM settings WHERE key = ?", "s3_bucket"); got != "imgs" {
		t.Fatalf("non-secret sealed or lost: %q", got)
	}
	st, err := s.Get(ctx, "s3_secret_access_key")
	if err != nil || st.Value != "super-secret" {
		t.Fatalf("read back = %+v, %v", st, err)
	}

	// Empty value deletes the row — the form's Clear.
	if err := s.SetMany(ctx, map[string]string{"s3_secret_access_key": "", "s3_bucket": ""}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, "s3_secret_access_key"); err == nil {
		t.Fatal("cleared key still stored")
	}
}
