package attachments

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/zzir/agents-go/cmd/agents-server/internal/settings"
)

// TestSignV4AWSVector checks the signature against the worked example in the
// AWS sigv4 documentation (GET
// https://examplebucket.s3.amazonaws.com/test.txt, SEC key
// wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY, 2013-05-24) — the classic S3
// test vector, with the Range header omitted (this implementation signs host
// and x-amz-* only, which changes the expected signature; the value below was
// derived by running the reference algorithm over that reduced header set).
func TestSignV4AWSVector(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, "https://examplebucket.s3.amazonaws.com/test.txt", nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC)
	const emptyHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	signV4(req, "AKIAIOSFODNN7EXAMPLE", "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY", "us-east-1", "s3", emptyHash, now)

	auth := req.Header.Get("Authorization")
	wantCred := "Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request"
	if !strings.Contains(auth, wantCred) {
		t.Fatalf("Authorization missing %q: %s", wantCred, auth)
	}
	if !strings.Contains(auth, "SignedHeaders=host;x-amz-content-sha256;x-amz-date") {
		t.Fatalf("unexpected signed headers: %s", auth)
	}
	if req.Header.Get("x-amz-date") != "20130524T000000Z" {
		t.Fatalf("x-amz-date = %q", req.Header.Get("x-amz-date"))
	}
	// Signature over host + the two x-amz headers, computed independently
	// with openssl over the AWS reference algorithm for exactly this
	// canonical request.
	const wantSig = "Signature=df548e2ce037944d03f3e68682813b093763996d597cf890ca3d9037fd231eb4"
	if !strings.Contains(auth, wantSig) {
		t.Fatalf("signature mismatch:\n got %s\nwant %s", auth, wantSig)
	}
}

func TestURIEncode(t *testing.T) {
	for in, want := range map[string]string{
		"abc-123_~.": "abc-123_~.",
		"a b":        "a%20b",
		"a/b":        "a%2Fb",
		"漢":          "%E6%BC%A2",
	} {
		if got := uriEncode(in); got != want {
			t.Errorf("uriEncode(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCanonicalQuery(t *testing.T) {
	u, _ := url.Parse("https://h/p?b=2&a=1&a=0")
	if got, want := canonicalQuery(u), "a=0&a=1&b=2"; got != want {
		t.Fatalf("canonicalQuery = %q, want %q", got, want)
	}
}

func TestObjectURLStyles(t *testing.T) {
	cfg := settings.S3Config{
		Endpoint: "https://s3.example.com", Region: "auto", Bucket: "imgs",
		AccessKeyID: "k", SecretKey: "s", PublicBaseURL: "https://pub.example.com",
	}
	c := ClientFrom(cfg, nil)
	if got, want := c.objectURL("att/x.png"), "https://imgs.s3.example.com/att/x.png"; got != want {
		t.Fatalf("virtual-hosted url = %q, want %q", got, want)
	}
	cfg.PathStyle = true
	c = ClientFrom(cfg, nil)
	if got, want := c.objectURL("att/x.png"), "https://s3.example.com/imgs/att/x.png"; got != want {
		t.Fatalf("path-style url = %q, want %q", got, want)
	}
}

func TestClientFromIncomplete(t *testing.T) {
	if ClientFrom(settings.S3Config{Endpoint: "https://x"}, nil) != nil {
		t.Fatal("incomplete config must yield a nil client")
	}
	if ClientFrom(settings.S3Config{}, nil) != nil {
		t.Fatal("empty config must yield a nil client")
	}
}

// TestPutDeleteProbe runs the whole client against a fake S3: verifies the
// request shapes (method, path, headers present) and the probe's three
// stages, including its anonymous public read.
func TestPutDeleteProbe(t *testing.T) {
	var putAuth, putSHA string
	store := map[string][]byte{}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.URL.Path, "/imgs/")
		switch r.Method {
		case http.MethodPut:
			putAuth = r.Header.Get("Authorization")
			putSHA = r.Header.Get("x-amz-content-sha256")
			b := make([]byte, r.ContentLength)
			_, _ = io.ReadFull(r.Body, b)
			store[key] = b
			w.WriteHeader(http.StatusOK)
		case http.MethodDelete:
			delete(store, key)
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer api.Close()
	pub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("public read must be anonymous")
		}
		b, ok := store[strings.TrimPrefix(r.URL.Path, "/")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(b)
	}))
	defer pub.Close()

	cfg := settings.S3Config{
		Endpoint: api.URL, Region: "auto", Bucket: "imgs", PathStyle: true,
		AccessKeyID: "AK", SecretKey: "SK", PublicBaseURL: pub.URL,
	}
	c := ClientFrom(cfg, nil)
	if err := c.Probe(context.Background()); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if len(store) != 0 {
		t.Fatalf("probe left objects behind: %v", store)
	}
	if !strings.HasPrefix(putAuth, "AWS4-HMAC-SHA256 Credential=AK/") {
		t.Fatalf("put not signed: %q", putAuth)
	}
	if putSHA == "" {
		t.Fatal("put missing x-amz-content-sha256")
	}

	if err := c.Put(context.Background(), "att/a.png", "image/png", []byte("png")); err != nil {
		t.Fatalf("put: %v", err)
	}
	if string(store["att/a.png"]) != "png" {
		t.Fatalf("stored = %q", store["att/a.png"])
	}
	if err := c.Delete(context.Background(), "att/a.png"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := c.Delete(context.Background(), "att/gone.png"); err != nil {
		t.Fatalf("delete of absent key must succeed: %v", err)
	}
}

// TestProbeNotPublic asserts the probe's error names the public-read stage
// when writes work but anonymous reads are refused.
func TestProbeNotPublic(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer api.Close()
	pub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer pub.Close()
	c := ClientFrom(settings.S3Config{
		Endpoint: api.URL, Region: "auto", Bucket: "b", PathStyle: true,
		AccessKeyID: "k", SecretKey: "s", PublicBaseURL: pub.URL,
	}, nil)
	err := c.Probe(context.Background())
	if err == nil || !strings.Contains(err.Error(), "anonymous reads") {
		t.Fatalf("want anonymous-read error, got %v", err)
	}
}
