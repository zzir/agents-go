package handler

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// skillCtx builds a gin context carrying a single:name path parameter, the way
// the /skill-repos/:name routes deliver it. gin URL-decodes params before they
// reach the handler, so a request path of "%2E" arrives here as ".".
func skillCtx(method, name string) (*gin.Context, *httptest.ResponseRecorder) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(method, "/", nil)
	c.Params = gin.Params{{Key: "name", Value: name}}
	return c, w
}

// DELETE/POST-sync of a repo name that resolves back to the skills root
// (".", "", "/", or an escape) must be rejected — never RemoveAll / git-reset
// the entire skills tree. "." is also the decoded form of the "%2E" URL input.
func TestSkillRepoRootDeleteGuard(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ws := t.TempDir()
	h := NewSkillHandler(ws)
	skillsDir := filepath.Join(ws, "skills")

	// A decoy repo whose survival proves the root was not wiped.
	decoy := filepath.Join(skillsDir, "keep")
	if err := os.MkdirAll(decoy, 0o755); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{".", "..", "", "/", "../", "./"} {
		c, w := skillCtx(http.MethodDelete, name)
		h.Delete(c)
		if w.Code != http.StatusBadRequest {
			t.Errorf("Delete(name=%q): status = %d, want 400", name, w.Code)
		}
		c, w = skillCtx(http.MethodPost, name)
		h.Sync(c)
		if w.Code != http.StatusBadRequest {
			t.Errorf("Sync(name=%q): status = %d, want 400", name, w.Code)
		}
	}

	if _, err := os.Stat(skillsDir); err != nil {
		t.Fatalf("skills root was removed by a guarded op: %v", err)
	}
	if _, err := os.Stat(decoy); err != nil {
		t.Fatalf("decoy repo was removed by a guarded op: %v", err)
	}
}

// A legitimate single-segment repo name still deletes normally (the guard must
// not over-reject). Routed through an engine so gin flushes the status-only 204.
func TestSkillRepoDeleteValid(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ws := t.TempDir()
	h := NewSkillHandler(ws)
	repo := filepath.Join(ws, "skills", "myrepo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	engine := gin.New()
	engine.DELETE("/skill-repos/:name", h.Delete)
	req := httptest.NewRequest(http.MethodDelete, "/skill-repos/myrepo", nil)
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("Delete(valid): status = %d, want 204", w.Code)
	}
	if _, err := os.Stat(repo); !os.IsNotExist(err) {
		t.Fatalf("valid repo was not removed: err = %v", err)
	}
}

// via the real router: the "%2E"-encoded "." must not wipe the skills root.
func TestSkillRepoRootDeleteGuardRouted(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ws := t.TempDir()
	h := NewSkillHandler(ws)
	skillsDir := filepath.Join(ws, "skills")
	if err := os.MkdirAll(filepath.Join(skillsDir, "keep"), 0o755); err != nil {
		t.Fatal(err)
	}
	engine := gin.New()
	engine.DELETE("/skill-repos/:name", h.Delete)

	for _, target := range []string{"/skill-repos/%2E", "/skill-repos/."} {
		req := httptest.NewRequest(http.MethodDelete, target, nil)
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, req)
		if w.Code >= 200 && w.Code < 300 {
			t.Errorf("DELETE %s: status = %d, want non-2xx", target, w.Code)
		}
	}
	if _, err := os.Stat(skillsDir); err != nil {
		t.Fatalf("skills root was removed via routed request: %v", err)
	}
}

// a SKILL.md that is a symlink escaping the repo (e.g. -> a file outside
// the skills tree) must not be read by Get, and the escaping skill must not
// surface in the listing — no external file content leaks either way.
func TestSkillSymlinkEscapeRefused(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ws := t.TempDir()
	// A secret file OUTSIDE the skills tree.
	secret := filepath.Join(ws, "secret.txt")
	if err := os.WriteFile(secret, []byte("TOPSECRET-DO-NOT-LEAK"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := NewSkillHandler(ws)
	skillsDir := filepath.Join(ws, "skills")

	// A legit skill, and an "evil" one whose SKILL.md symlinks to the secret.
	good := filepath.Join(skillsDir, "good")
	if err := os.MkdirAll(good, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(good, "SKILL.md"), []byte("---\ndescription: real skill\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	evil := filepath.Join(skillsDir, "evil")
	if err := os.MkdirAll(evil, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(evil, "SKILL.md")); err != nil {
		t.Skipf("symlinks unsupported on this platform: %v", err)
	}

	// Get on the escaping skill: 404, and the secret must not appear in the body.
	c, w := skillCtx(http.MethodGet, "")
	c.Params = gin.Params{{Key: "path", Value: "/evil"}}
	h.Get(c)
	if w.Code != http.StatusNotFound {
		t.Errorf("Get(evil): status = %d, want 404", w.Code)
	}
	if strings.Contains(w.Body.String(), "TOPSECRET") {
		t.Errorf("Get leaked the escaping symlink target: %s", w.Body.String())
	}

	// Listing skips the escaping symlink and never reads its target.
	skills := findAllSkills(skillsDir)
	names := map[string]bool{}
	for _, s := range skills {
		names[s.Name] = true
		if strings.Contains(s.Description, "TOPSECRET") {
			t.Errorf("findAllSkills leaked the symlink target in a description: %+v", s)
		}
	}
	if !names["good"] {
		t.Errorf("findAllSkills dropped the legit skill: %+v", skills)
	}
	if names["evil"] {
		t.Errorf("findAllSkills listed the escaping symlink skill: %+v", skills)
	}

	// The legit skill still reads back through the rooted path.
	c, w = skillCtx(http.MethodGet, "")
	c.Params = gin.Params{{Key: "path", Value: "/good"}}
	h.Get(c)
	if w.Code != http.StatusOK {
		t.Fatalf("Get(good): status = %d, want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), "real skill") {
		t.Errorf("Get(good) body missing content: %s", w.Body.String())
	}
}
