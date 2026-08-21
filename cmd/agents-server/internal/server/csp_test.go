package server

import (
	"bytes"
	"compress/gzip"
	"strings"
	"testing"
	"testing/fstest"
)

const cspTestPage = `<html><head><script>var x = 1;</script></head><body><script type="module" src="/a.js"></script></body></html>`

// The known-good hash of `var x = 1;`.
const cspTestHash = "9nfWt3DNT14o+tZCP3YilfLwTrhLI98eqbN689B7ajY="

func TestInlineScriptHashes(t *testing.T) {
	plain := fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte(cspTestPage)}}
	got := inlineScriptHashes(plain)
	if len(got) != 1 || got[0] != cspTestHash {
		t.Fatalf("plain index.html: got %v, want [%s]", got, cspTestHash)
	}

	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write([]byte(cspTestPage)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	gzipped := fstest.MapFS{"index.html.gz": &fstest.MapFile{Data: buf.Bytes()}}
	got = inlineScriptHashes(gzipped)
	if len(got) != 1 || got[0] != cspTestHash {
		t.Fatalf("index.html.gz: got %v, want [%s]", got, cspTestHash)
	}

	if got := inlineScriptHashes(fstest.MapFS{}); got != nil {
		t.Fatalf("missing index: got %v, want nil", got)
	}
}

func TestBuildCSP(t *testing.T) {
	base := buildCSP(nil)
	if !strings.Contains(base, "script-src 'self';") {
		t.Fatalf("base policy lost script-src 'self': %q", base)
	}
	withHash := buildCSP([]string{cspTestHash})
	if !strings.Contains(withHash, "script-src 'self' 'sha256-"+cspTestHash+"';") {
		t.Fatalf("hashed policy malformed: %q", withHash)
	}
}
