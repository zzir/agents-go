package handler

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/zzir/agents-go/cmd/agents-server/internal/sandboxes"
	"github.com/zzir/agents-go/cmd/agents-server/internal/server"
	"github.com/zzir/agents-go/cmd/agents-server/internal/settings"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
	"github.com/zzir/agents-go/cmd/agents-server/internal/testdb"
	"github.com/zzir/agents-go/sandbox"
)

// previewSandbox stands in for a backend whose ports live somewhere this
// process can reach: the test's own HTTP server.
type previewSandbox struct {
	sandbox.Sandbox
	base string
}

func (p previewSandbox) URLForPort(context.Context, int) (string, error) { return p.base, nil }

func (p previewSandbox) DialPort(ctx context.Context, _ int) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "tcp", strings.TrimPrefix(p.base, "http://"))
}

// previewRequest builds a request whose context can be cancelled.
// httputil.ReverseProxy falls back to http.CloseNotifier when the request
// context has no Done channel, and httptest's recorder has no CloseNotify —
// a real server request always has one.
func previewRequest(t *testing.T, url string) *http.Request {
	t.Helper()
	return httptest.NewRequest(http.MethodGet, url, nil).WithContext(t.Context())
}

// previewFixture stands up a project whose sandbox is upstream, with previews
// on or off.
func previewFixture(t *testing.T, upstream string, enabled bool) (*gin.Engine, *ProjectHandler, *store.Project) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db := testdb.New(t)
	projects := store.NewProjectStore(db)
	targetID, templateID := mkSandboxRows(t, db)
	p := &store.Project{ID: store.NewID(), OwnerID: store.LocalUserID, TargetID: targetID, TemplateID: templateID, Name: "p"}
	if err := projects.Create(t.Context(), p); err != nil {
		t.Fatal(err)
	}
	mgr := sandboxes.NewManager()
	mgr.SetBuildOverride(func(sandboxes.Spec) (sandbox.Sandbox, error) {
		return previewSandbox{base: upstream}, nil
	})
	settingStore := store.NewSettingStore(db)
	if enabled {
		if err := settingStore.Set(t.Context(), settings.KeyPreviewEnabled, "true"); err != nil {
			t.Fatal(err)
		}
	}
	reader := settings.NewReader(settingStore)
	targets, templates := store.NewSandboxTargetStore(db), store.NewSandboxTemplateStore(db)
	h := NewProjectHandler(projects, targets, templates, mgr,
		NewTerminalHandler(targets, templates, projects, mgr, reader), reader)

	e := newTestEngine()
	e.POST("/api/v1/projects/:id/preview/:port", h.PreviewGrant)
	e.Any(server.PreviewPrefix+":token/*path", h.Preview)
	return e, h, p
}

// The whole path: a grant is minted through the authenticated API, and the
// URL it hands back reaches the service WITHOUT a bearer token — which is the
// point, since a browser tab has none.
func TestPreviewGrantAndProxy(t *testing.T) {
	var gotPath, gotAuth, gotCookie string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth, gotCookie = r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("Cookie")
		_, _ = w.Write([]byte("hello from the sandbox"))
	}))
	defer upstream.Close()

	e, _, p := previewFixture(t, upstream.URL, true)
	rec := doJSON(t, e, http.MethodPost, "/api/v1/projects/"+p.ID+"/preview/8000", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("grant: %d %s", rec.Code, rec.Body)
	}
	var grant previewGrantResp
	if err := json.Unmarshal(rec.Body.Bytes(), &grant); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(grant.URL, server.PreviewPrefix) {
		t.Fatalf("grant url = %q, want it under %s", grant.URL, server.PreviewPrefix)
	}

	// No Authorization header on the way in — and the app's own credential
	// must not be forwarded even when one is present.
	req := previewRequest(t, grant.URL+"assets/app.js")
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Cookie", "session=secret")
	out := httptest.NewRecorder()
	e.ServeHTTP(out, req)
	if out.Code != http.StatusOK {
		t.Fatalf("preview: %d %s", out.Code, out.Body)
	}
	if out.Body.String() != "hello from the sandbox" {
		t.Fatalf("body = %q", out.Body.String())
	}
	if gotPath != "/assets/app.js" {
		t.Errorf("upstream saw %q, want the path below the grant", gotPath)
	}
	if gotAuth != "" || gotCookie != "" {
		t.Errorf("the workbench's credential reached the previewed service: auth=%q cookie=%q", gotAuth, gotCookie)
	}
}

// An unknown grant is a 404, not a proxy attempt.
func TestPreviewUnknownGrant(t *testing.T) {
	e, _, _ := previewFixture(t, "http://127.0.0.1:1", true)
	req := previewRequest(t, server.PreviewPrefix+"nope/")
	out := httptest.NewRecorder()
	e.ServeHTTP(out, req)
	if out.Code != http.StatusNotFound {
		t.Fatalf("unknown grant = %d, want 404", out.Code)
	}
}

// Previews are off by default: the feature makes whatever is listening inside
// a sandbox reachable by anyone who can sign in, so an operator turns it on.
func TestPreviewDisabledByDefault(t *testing.T) {
	e, _, p := previewFixture(t, "http://127.0.0.1:1", false)
	rec := doJSON(t, e, http.MethodPost, "/api/v1/projects/"+p.ID+"/preview/8000", "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("grant with previews off = %d, want 403", rec.Code)
	}
}

// A grant names one project and one port, and dies with the project.
func TestPreviewGrantRevokedWithTheProject(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) }))
	defer upstream.Close()
	e, h, p := previewFixture(t, upstream.URL, true)
	rec := doJSON(t, e, http.MethodPost, "/api/v1/projects/"+p.ID+"/preview/8000", "")
	var grant previewGrantResp
	if err := json.Unmarshal(rec.Body.Bytes(), &grant); err != nil {
		t.Fatal(err)
	}
	h.grants.revokeProject(p.ID)
	req := previewRequest(t, grant.URL)
	out := httptest.NewRecorder()
	e.ServeHTTP(out, req)
	if out.Code != http.StatusNotFound {
		t.Fatalf("revoked grant = %d, want 404", out.Code)
	}
}

// A bad port is refused at the mint, not discovered at the proxy.
func TestPreviewPortValidated(t *testing.T) {
	e, _, p := previewFixture(t, "http://127.0.0.1:1", true)
	for _, port := range []string{"0", "70000", "abc"} {
		if rec := doJSON(t, e, http.MethodPost, "/api/v1/projects/"+p.ID+"/preview/"+port, ""); rec.Code != http.StatusBadRequest {
			t.Errorf("port %q = %d, want 400", port, rec.Code)
		}
	}
}
