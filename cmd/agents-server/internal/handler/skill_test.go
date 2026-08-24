package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/zzir/agents-go/cmd/agents-server/internal/settings"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
	"github.com/zzir/agents-go/cmd/agents-server/internal/testdb"
)

const pdfSkillDoc = `---
name: pdf-processing
description: Extract text and fill PDF forms.
---

# PDF processing
Step 1.
`

// skillTestEnv mounts the skill routes the way routes.go does and returns the
// handler for endpoint overrides.
func skillTestEnv(t *testing.T) (*gin.Engine, *SkillHandler, *store.SkillStore) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db := testdb.New(t)
	st := store.NewSkillStore(db)
	h := NewSkillHandler(st, settings.NewReader(store.NewSettingStore(db)))
	engine := newTestEngine()
	engine.GET("/skills", h.List)
	engine.GET("/skills/:id", h.Get)
	engine.POST("/skills", h.Create)
	engine.PUT("/skills/:id", h.Update)
	engine.DELETE("/skills/:id", h.Delete)
	engine.POST("/skill-imports", h.Import)
	return engine, h, st
}

func skillBody(content string) string {
	b, _ := json.Marshal(map[string]string{"content": content})
	return string(b)
}

// Create parses the document's frontmatter into the row; List answers without
// content; Get returns the full document; Update follows the new frontmatter.
func TestSkillCrud(t *testing.T) {
	engine, _, _ := skillTestEnv(t)

	w := doJSON(t, engine, http.MethodPost, "/skills", skillBody(pdfSkillDoc))
	if w.Code != http.StatusCreated {
		t.Fatalf("create: got %d (body %s)", w.Code, w.Body.String())
	}
	var sk store.Skill
	if err := json.Unmarshal(w.Body.Bytes(), &sk); err != nil {
		t.Fatal(err)
	}
	if sk.Name != "pdf-processing" || !strings.HasPrefix(sk.Description, "Extract text") {
		t.Fatalf("frontmatter not parsed into row: %+v", sk)
	}

	if w := doJSON(t, engine, http.MethodPost, "/skills", skillBody(pdfSkillDoc)); w.Code != http.StatusConflict {
		t.Fatalf("duplicate name: got %d, want 409", w.Code)
	}

	w = doJSON(t, engine, http.MethodGet, "/skills", "")
	var list []store.Skill
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Content != "" {
		t.Fatalf("list must carry metadata only: %+v", list)
	}

	w = doJSON(t, engine, http.MethodGet, "/skills/"+sk.ID, "")
	var got store.Skill
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got.Content != pdfSkillDoc {
		t.Fatalf("get lost the content: %q", got.Content)
	}

	renamed := strings.Replace(pdfSkillDoc, "pdf-processing", "pdf-tools", 1)
	w = doJSON(t, engine, http.MethodPut, "/skills/"+sk.ID, skillBody(renamed))
	if w.Code != http.StatusOK {
		t.Fatalf("update: got %d (body %s)", w.Code, w.Body.String())
	}
	var upd store.Skill
	_ = json.Unmarshal(w.Body.Bytes(), &upd)
	if upd.Name != "pdf-tools" || upd.Detached {
		t.Fatalf("update result wrong (a local skill never detaches): %+v", upd)
	}

	if w := doJSON(t, engine, http.MethodDelete, "/skills/"+sk.ID, ""); w.Code != http.StatusNoContent {
		t.Fatalf("delete: got %d", w.Code)
	}
	if w := doJSON(t, engine, http.MethodGet, "/skills/"+sk.ID, ""); w.Code != http.StatusNotFound {
		t.Fatalf("get after delete: got %d, want 404", w.Code)
	}
}

// Save-time validation: a document that is not a valid SKILL.md, or over the
// size cap, never lands in the table.
func TestSkillCreateRejectsInvalid(t *testing.T) {
	engine, _, _ := skillTestEnv(t)
	cases := []struct {
		name    string
		content string
		wantSub string
	}{
		{"no frontmatter", "# nope", "invalid SKILL.md"},
		{"bad name", "---\nname: NOPE\ndescription: d\n---\nx", "invalid SKILL.md"},
		{"oversize", "---\nname: big\ndescription: d\n---\n" + strings.Repeat("x", maxSkillBytes), "exceeds"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := doJSON(t, engine, http.MethodPost, "/skills", skillBody(tc.content))
			if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), tc.wantSub) {
				t.Fatalf("got %d %s", w.Code, w.Body.String())
			}
		})
	}
}

// fakeGitHub serves the three endpoints an import walks: HEAD commit, tree
// listing, raw file content. files maps repo path -> SKILL.md content.
func fakeGitHub(t *testing.T, sha string, files map[string]string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r/commits/HEAD", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"sha": sha})
	})
	mux.HandleFunc("/repos/o/r/git/trees/"+sha, func(w http.ResponseWriter, _ *http.Request) {
		var tree []map[string]string
		for p := range files {
			tree = append(tree, map[string]string{"path": p, "type": "blob"})
		}
		tree = append(tree, map[string]string{"path": "README.md", "type": "blob"})
		_ = json.NewEncoder(w).Encode(map[string]any{"tree": tree, "truncated": false})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// raw.githubusercontent form: /o/r/<sha>/<path>
		prefix := "/o/r/" + sha + "/"
		if strings.HasPrefix(r.URL.Path, prefix) {
			if content, ok := files[strings.TrimPrefix(r.URL.Path, prefix)]; ok {
				_, _ = fmt.Fprint(w, content)
				return
			}
		}
		http.NotFound(w, r)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func importGitHub(t *testing.T, engine *gin.Engine) skillImportResp {
	t.Helper()
	w := doJSON(t, engine, http.MethodPost, "/skill-imports", `{"url":"https://github.com/o/r"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("import: got %d (body %s)", w.Code, w.Body.String())
	}
	var resp skillImportResp
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	return resp
}

// A GitHub repo import walks the tree for SKILL.md files at any depth,
// creates rows with their source, refreshes an unedited row on re-import,
// and never overwrites one edited locally (detached).
func TestSkillImportGitHubLifecycle(t *testing.T) {
	engine, h, st := skillTestEnv(t)
	files := map[string]string{
		"pdf/SKILL.md":         pdfSkillDoc,
		"nested/docx/SKILL.md": "---\nname: docx\ndescription: Word documents.\n---\nBody v1.\n",
	}
	gh := fakeGitHub(t, "sha1", files)
	h.githubAPI, h.githubRaw = gh.URL, gh.URL

	resp := importGitHub(t, engine)
	if len(resp.Created) != 2 {
		t.Fatalf("created = %v", resp)
	}
	sk, err := st.GetByName(t.Context(), "docx")
	if err != nil {
		t.Fatal(err)
	}
	if sk.SourceRepo != "https://github.com/o/r" || sk.SourcePath != "nested/docx/SKILL.md" || sk.SourceSHA != "sha1" {
		t.Fatalf("source not recorded: %+v", sk)
	}

	// Unchanged re-import touches nothing.
	resp = importGitHub(t, engine)
	if len(resp.Unchanged) != 2 || len(resp.Created)+len(resp.Updated) != 0 {
		t.Fatalf("re-import = %+v", resp)
	}

	// Upstream change refreshes the row.
	files["nested/docx/SKILL.md"] = "---\nname: docx\ndescription: Word documents.\n---\nBody v2.\n"
	resp = importGitHub(t, engine)
	if len(resp.Updated) != 1 || resp.Updated[0] != "docx" {
		t.Fatalf("after upstream change = %+v", resp)
	}

	// A local edit detaches; the next import must not overwrite it.
	local := "---\nname: docx\ndescription: Word documents.\n---\nMY EDIT.\n"
	if w := doJSON(t, engine, http.MethodPut, "/skills/"+sk.ID, skillBody(local)); w.Code != http.StatusOK {
		t.Fatalf("edit: %d %s", w.Code, w.Body.String())
	}
	files["nested/docx/SKILL.md"] = "---\nname: docx\ndescription: Word documents.\n---\nBody v3.\n"
	resp = importGitHub(t, engine)
	if len(resp.Skipped) != 1 || !strings.Contains(resp.Skipped[0], "detached") {
		t.Fatalf("detached skill was not protected: %+v", resp)
	}
	sk, _ = st.GetByName(t.Context(), "docx")
	if sk.Content != local || !sk.Detached {
		t.Fatalf("local edit lost: %+v", sk)
	}
}

// A name collision with an existing skill from elsewhere is skipped with a
// reason, not an overwrite and not a hard failure for the rest of the import.
func TestSkillImportNameCollision(t *testing.T) {
	engine, h, _ := skillTestEnv(t)
	if w := doJSON(t, engine, http.MethodPost, "/skills", skillBody(pdfSkillDoc)); w.Code != http.StatusCreated {
		t.Fatalf("seed: %d", w.Code)
	}
	gh := fakeGitHub(t, "sha1", map[string]string{"pdf/SKILL.md": pdfSkillDoc})
	h.githubAPI, h.githubRaw = gh.URL, gh.URL
	resp := importGitHub(t, engine)
	if len(resp.Skipped) != 1 || !strings.Contains(resp.Skipped[0], "already in use") {
		t.Fatalf("collision = %+v", resp)
	}
}

// Any non-repo http(s) URL imports as a single raw SKILL.md.
func TestSkillImportRawURL(t *testing.T) {
	engine, _, st := skillTestEnv(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, pdfSkillDoc)
	}))
	t.Cleanup(srv.Close)

	w := doJSON(t, engine, http.MethodPost, "/skill-imports", `{"url":"`+srv.URL+`/pdf/SKILL.md"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("raw import: got %d (body %s)", w.Code, w.Body.String())
	}
	sk, err := st.GetByName(t.Context(), "pdf-processing")
	if err != nil {
		t.Fatal(err)
	}
	if sk.SourceRepo != srv.URL+"/pdf/SKILL.md" || sk.SourcePath != "" {
		t.Fatalf("raw source not recorded: %+v", sk)
	}
}

// A repository with no SKILL.md anywhere is the caller's mistake (400), not a
// silent empty success.
func TestSkillImportEmptyRepo(t *testing.T) {
	engine, h, _ := skillTestEnv(t)
	gh := fakeGitHub(t, "sha1", nil)
	h.githubAPI, h.githubRaw = gh.URL, gh.URL
	w := doJSON(t, engine, http.MethodPost, "/skill-imports", `{"url":"https://github.com/o/r"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("empty repo: got %d (body %s)", w.Code, w.Body.String())
	}
}
