package store

import (
	"context"
	"errors"
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
