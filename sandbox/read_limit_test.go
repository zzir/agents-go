package sandbox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadAllLimited(t *testing.T) {
	t.Run("under limit", func(t *testing.T) {
		data, err := ReadAllLimited(strings.NewReader("hello"), 16)
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != "hello" {
			t.Errorf("data = %q", data)
		}
	})
	t.Run("exactly at limit", func(t *testing.T) {
		data, err := ReadAllLimited(strings.NewReader("12345678"), 8)
		if err != nil {
			t.Fatalf("content exactly at the limit must succeed: %v", err)
		}
		if len(data) != 8 {
			t.Errorf("len = %d, want 8", len(data))
		}
	})
	t.Run("over limit", func(t *testing.T) {
		_, err := ReadAllLimited(strings.NewReader("123456789"), 8)
		if err == nil {
			t.Fatal("expected an error for content over the limit")
		}
		if !errors.Is(err, ErrReadLimitExceeded) {
			t.Errorf("err = %v, want ErrReadLimitExceeded in the chain", err)
		}
		if !strings.Contains(err.Error(), "file exceeds read limit (8 bytes)") {
			t.Errorf("err = %q, want it to name the limit", err)
		}
	})
	t.Run("zero limit uses default", func(t *testing.T) {
		// A small file must pass with limit 0 (= DefaultMaxReadFileBytes),
		// proving 0 is "default", not "nothing allowed".
		data, err := ReadAllLimited(strings.NewReader("ok"), 0)
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != "ok" {
			t.Errorf("data = %q", data)
		}
	})
	t.Run("negative limit uses default", func(t *testing.T) {
		data, err := ReadAllLimited(strings.NewReader("ok"), -1)
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != "ok" {
			t.Errorf("data = %q", data)
		}
	})
}

func TestLocalSandbox_ReadFileLimit(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "big.bin"), make([]byte, 100), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "small.txt"), []byte("fits"), 0o644); err != nil {
		t.Fatal(err)
	}

	sb := NewLocalWithOptions(LocalOptions{WorkDir: dir, MaxReadFileBytes: 16})
	ctx := context.Background()

	if _, err := sb.ReadFile(ctx, "big.bin"); !errors.Is(err, ErrReadLimitExceeded) {
		t.Errorf("ReadFile(big.bin) err = %v, want ErrReadLimitExceeded", err)
	}
	data, err := sb.ReadFile(ctx, "small.txt")
	if err != nil {
		t.Fatalf("ReadFile(small.txt): %v", err)
	}
	if string(data) != "fits" {
		t.Errorf("data = %q", data)
	}
}

func TestLocalSandbox_ReadFileDefaultLimit(t *testing.T) {
	// With MaxReadFileBytes unset, files under the 8 MiB default still read fine.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("default ok"), 0o644); err != nil {
		t.Fatal(err)
	}
	sb := NewLocalWithOptions(LocalOptions{WorkDir: dir})
	data, err := sb.ReadFile(context.Background(), "f.txt")
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "default ok" {
		t.Errorf("data = %q", data)
	}
}
