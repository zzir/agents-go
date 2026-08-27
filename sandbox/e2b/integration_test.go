//go:build e2b_integration

// Run against a real service with:
//
//	E2B_API_KEY=… E2B_TEMPLATE=… go test -tags e2b_integration ./sandbox/e2b
//	E2B_API_KEY=… E2B_API_URL=… E2B_DOMAIN=… E2B_TEMPLATE=… go test -tags e2b_integration ./sandbox/e2b
//
// It creates and kills one sandbox per subtest, and it costs money. The build
// tag keeps it out of CI, which has no credentials and should not spend any.
package e2b_test

import (
	"context"
	"os"
	"testing"

	"github.com/zzir/agents-go/sandbox"
	"github.com/zzir/agents-go/sandbox/e2b"
	"github.com/zzir/agents-go/sandbox/sandboxtest"
)

// The conformance suite against the real thing — the only check that the
// service agrees with this client's reading of its API.
func TestRealServiceConformance(t *testing.T) {
	key := os.Getenv("E2B_API_KEY")
	template := os.Getenv("E2B_TEMPLATE")
	if key == "" || template == "" {
		t.Skip("set E2B_API_KEY and E2B_TEMPLATE to run this")
	}
	sandboxtest.Run(t, func(t *testing.T) sandbox.Sandbox {
		t.Helper()
		sb, err := e2b.New(e2b.Options{
			APIURL:     os.Getenv("E2B_API_URL"),
			Domain:     os.Getenv("E2B_DOMAIN"),
			APIKey:     key,
			TemplateID: template,
			// The suite writes relative paths; E2B sandboxes run as "user",
			// whose home is where a template puts anything anyway.
			WorkDir: "/home/user",
			// Not every service takes autoPause — Alibaba Cloud gates it on a
			// per-function feature — and the suite does not depend on it.
			AutoPause: os.Getenv("E2B_AUTO_PAUSE") == "1",
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			// context.Background(), not t.Context(): the test's context is
			// already cancelled when cleanups run, and a cancelled DELETE
			// leaves a sandbox billing until its lease expires.
			if derr := sb.Destroy(context.Background()); derr != nil {
				t.Logf("destroying the sandbox: %v", derr)
			}
		})
		return sb
	})
}
