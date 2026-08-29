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

// Close overrides the embedded nil interface's: a config change closes the
// cached instance.
func (p previewSandbox) Close() error { return nil }

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
	sandboxID := mkSandboxRow(t, db)
	p := &store.Project{ID: store.NewID(), OwnerID: store.LocalUserID, SandboxID: sandboxID, Name: "p"}
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
	sbs := store.NewSandboxStore(db)
	h := NewProjectHandler(projects, sbs, mgr,
		NewTerminalHandler(sbs, projects, mgr, reader), reader)

	e := newTestEngine()
	e.POST("/api/v1/projects/:id/preview/:port", h.PreviewGrant)
	e.PUT("/api/v1/projects/:id", h.Update)
	e.Any(server.PreviewPrefix+":token/*path", h.Preview)
	e.NoRoute(h.Preview) // the cookie route for a page's absolute-path sub-resources
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

// A previewed page's absolute-path sub-resources carry no token in the path;
// the cookie the tokenized entry point planted routes them to the same grant,
// and without that cookie they are a plain 404.
func TestPreviewCookieRoutesAbsolutePaths(t *testing.T) {
	var gotPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte("asset"))
	}))
	defer upstream.Close()

	e, _, p := previewFixture(t, upstream.URL, true)
	rec := doJSON(t, e, http.MethodPost, "/api/v1/projects/"+p.ID+"/preview/8000", "")
	var grant previewGrantResp
	if err := json.Unmarshal(rec.Body.Bytes(), &grant); err != nil {
		t.Fatal(err)
	}

	// The tokenized entry point plants the cookie.
	out := httptest.NewRecorder()
	e.ServeHTTP(out, previewRequest(t, grant.URL))
	if out.Code != http.StatusOK {
		t.Fatalf("tokenized entry: %d %s", out.Code, out.Body)
	}
	var token string
	for _, ck := range out.Result().Cookies() {
		if ck.Name == previewCookie {
			token = ck.Value
		}
	}
	if token == "" {
		t.Fatal("the tokenized entry point did not plant the preview cookie")
	}

	// An absolute-path request carrying only that cookie routes to the grant.
	req := previewRequest(t, "/app.js")
	req.AddCookie(&http.Cookie{Name: previewCookie, Value: token})
	out2 := httptest.NewRecorder()
	e.ServeHTTP(out2, req)
	if out2.Code != http.StatusOK {
		t.Fatalf("cookie route: %d %s", out2.Code, out2.Body)
	}
	if gotPath != "/app.js" {
		t.Errorf("upstream saw %q, want /app.js", gotPath)
	}

	// Without the cookie the same path has no grant: a plain 404.
	out3 := httptest.NewRecorder()
	e.ServeHTTP(out3, previewRequest(t, "/app.js"))
	if out3.Code != http.StatusNotFound {
		t.Errorf("no-cookie absolute path = %d, want 404", out3.Code)
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

// A content change replaces the project's container, so it revokes the
// project's grants along with its terminals — an outstanding grant must not
// proxy into the replacement. A rename replaces nothing and revokes nothing.
func TestPreviewGrantRevokedByConfigChange(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) }))
	defer upstream.Close()
	e, _, p := previewFixture(t, upstream.URL, true)
	rec := doJSON(t, e, http.MethodPost, "/api/v1/projects/"+p.ID+"/preview/8000", "")
	var grant previewGrantResp
	if err := json.Unmarshal(rec.Body.Bytes(), &grant); err != nil {
		t.Fatal(err)
	}

	// A rename is not a content change: the grant survives it.
	if rec := doJSON(t, e, http.MethodPut, "/api/v1/projects/"+p.ID,
		`{"name":"renamed","sandbox_id":"`+p.SandboxID+`"}`); rec.Code != http.StatusOK {
		t.Fatalf("rename: %d %s", rec.Code, rec.Body)
	}
	out := httptest.NewRecorder()
	e.ServeHTTP(out, previewRequest(t, grant.URL))
	if out.Code != http.StatusOK {
		t.Fatalf("grant after a rename = %d, want 200", out.Code)
	}

	// An environment change replaces the container: the grant dies with it.
	if rec := doJSON(t, e, http.MethodPut, "/api/v1/projects/"+p.ID,
		`{"name":"renamed","sandbox_id":"`+p.SandboxID+`","env":[{"key":"A","value":"1"}]}`); rec.Code != http.StatusOK {
		t.Fatalf("env update: %d %s", rec.Code, rec.Body)
	}
	out = httptest.NewRecorder()
	e.ServeHTTP(out, previewRequest(t, grant.URL))
	if out.Code != http.StatusNotFound {
		t.Fatalf("grant after a config change = %d, want 404", out.Code)
	}
}

// A sandbox-side content change reaches every project on it through the
// Retirer, grants included.
func TestRetirerBumpRevokesPreviewGrants(t *testing.T) {
	db := testdb.New(t)
	projects := store.NewProjectStore(db)
	sandboxID := mkSandboxRow(t, db)
	p := &store.Project{ID: store.NewID(), OwnerID: store.LocalUserID, SandboxID: sandboxID, Name: "p"}
	if err := projects.Create(t.Context(), p); err != nil {
		t.Fatal(err)
	}
	var revoked []string
	sbs := store.NewSandboxStore(db)
	r := NewRetirer(projects, nil, NewTerminalHandler(sbs, projects, nil, settings.NewReader(nil)),
		func(id string) { revoked = append(revoked, id) })
	if err := r.bump(t.Context(), sandboxID); err != nil {
		t.Fatal(err)
	}
	if len(revoked) != 1 || revoked[0] != p.ID {
		t.Fatalf("revoked = %v, want [%s]", revoked, p.ID)
	}
}

// Behind --preview-base-url https://… a TLS-terminating proxy fronts the
// preview: the request arrives over plain HTTP (r.TLS nil) but the browser
// speaks https, so the grant cookie must still carry Secure.
func TestPreviewCookieSecureBehindHTTPSBase(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) }))
	defer upstream.Close()
	e, h, p := previewFixture(t, upstream.URL, true)
	h.PreviewOrigin = PreviewOrigin{BaseURL: "https://preview.example", Port: 9528}
	rec := doJSON(t, e, http.MethodPost, "/api/v1/projects/"+p.ID+"/preview/8000", "")
	var grant previewGrantResp
	if err := json.Unmarshal(rec.Body.Bytes(), &grant); err != nil {
		t.Fatal(err)
	}
	out := httptest.NewRecorder()
	e.ServeHTTP(out, previewRequest(t, grant.URL))
	if out.Code != http.StatusOK {
		t.Fatalf("tokenized entry: %d %s", out.Code, out.Body)
	}
	for _, ck := range out.Result().Cookies() {
		if ck.Name == previewCookie {
			if !ck.Secure {
				t.Error("grant cookie behind an https preview base is not Secure")
			}
			return
		}
	}
	t.Fatal("the tokenized entry point did not plant the preview cookie")
}

// The grant URL opens on the preview ORIGIN, not the app's: a fixed base when
// one is configured, otherwise the request host with the preview port. A page
// on that origin cannot read the app origin's stored token (decisions §5.37).
func TestPreviewOriginURL(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "http://work.example:9527/api/v1/projects/p/preview/8000", nil)
	cases := []struct {
		name   string
		origin PreviewOrigin
		want   string
	}{
		{"derived from request host and preview port", PreviewOrigin{Port: 9528}, "http://work.example:9528" + server.PreviewPrefix + "tok/"},
		{"fixed base for a reverse proxy", PreviewOrigin{BaseURL: "https://preview.example", Port: 9528}, "https://preview.example" + server.PreviewPrefix + "tok/"},
		{"unconfigured falls back to a relative path", PreviewOrigin{}, server.PreviewPrefix + "tok/"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.origin.urlFor(req, "tok"); got != tc.want {
				t.Errorf("urlFor = %q, want %q", got, tc.want)
			}
		})
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
