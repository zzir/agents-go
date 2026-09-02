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
	// The model-facing name of an imported skill carries its repo (decisions §5.29).
	sk, err := st.GetByNameFor(t.Context(), "o/r:docx", store.LocalUserID)
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
	sk, _ = st.GetByNameFor(t.Context(), "o/r:docx", store.LocalUserID)
	if sk.Content != local || !sk.Detached {
		t.Fatalf("local edit lost: %+v", sk)
	}
}

// A repo is its skills' namespace: an import whose frontmatter name matches a
// workbench-authored skill lands beside it rather than colliding, and the two
// answer to different model-facing names (decisions §5.29).
func TestSkillImportNamespacedByRepo(t *testing.T) {
	engine, h, st := skillTestEnv(t)
	if w := doJSON(t, engine, http.MethodPost, "/skills", skillBody(pdfSkillDoc)); w.Code != http.StatusCreated {
		t.Fatalf("seed: %d", w.Code)
	}
	gh := fakeGitHub(t, "sha1", map[string]string{"pdf/SKILL.md": pdfSkillDoc})
	h.githubAPI, h.githubRaw = gh.URL, gh.URL
	resp := importGitHub(t, engine)
	if len(resp.Created) != 1 || len(resp.Skipped) != 0 {
		t.Fatalf("import beside a same-named local skill = %+v", resp)
	}
	local, err := st.GetByNameFor(t.Context(), "pdf-processing", store.LocalUserID)
	if err != nil || local.SourceRepo != "" {
		t.Fatalf("bare name must resolve to the workbench-authored skill: (%+v, %v)", local, err)
	}
	imported, err := st.GetByNameFor(t.Context(), "o/r:pdf-processing", store.LocalUserID)
	if err != nil || imported.SourceRepo != "https://github.com/o/r" {
		t.Fatalf("qualified name must resolve to the imported skill: (%+v, %v)", imported, err)
	}
}

// Two skills of one repo sharing a frontmatter name DO collide — the repo is
// the namespace, so the second is skipped with a reason rather than
// overwriting the first or failing the whole import.
func TestSkillImportNameCollisionWithinRepo(t *testing.T) {
	engine, h, _ := skillTestEnv(t)
	gh := fakeGitHub(t, "sha1", map[string]string{
		"pdf/SKILL.md":   pdfSkillDoc,
		"other/SKILL.md": pdfSkillDoc,
	})
	h.githubAPI, h.githubRaw = gh.URL, gh.URL
	resp := importGitHub(t, engine)
	if len(resp.Created) != 1 || len(resp.Skipped) != 1 || !strings.Contains(resp.Skipped[0], "already in use") {
		t.Fatalf("collision within one repo = %+v", resp)
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
	// A raw-URL import is namespaced by its host, not an owner/repo pair.
	host := strings.TrimPrefix(srv.URL, "http://")
	sk, err := st.GetByNameFor(t.Context(), host+":pdf-processing", store.LocalUserID)
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

// A repo group is one scope: publishing flips every one of its skills at
// once, an imported skill refuses a flip of its own, and a later sync's NEW
// files join the group rather than splitting it (decisions §5.29).
func TestSkillRepoScopeGroup(t *testing.T) {
	engine, h, st := skillTestEnv(t)
	engine.POST("/skills/:id/scope", h.SetScope)
	engine.POST("/skill-repos/scope", h.SetRepoScope)
	files := map[string]string{
		"pdf/SKILL.md":  pdfSkillDoc,
		"docx/SKILL.md": "---\nname: docx\ndescription: Word documents.\n---\nBody.\n",
	}
	gh := fakeGitHub(t, "sha1", files)
	h.githubAPI, h.githubRaw = gh.URL, gh.URL
	if resp := importGitHub(t, engine); len(resp.Created) != 2 {
		t.Fatalf("import = %+v", resp)
	}

	// One imported skill cannot leave its group alone.
	one, err := st.GetByNameFor(t.Context(), "o/r:docx", store.LocalUserID)
	if err != nil {
		t.Fatal(err)
	}
	if w := doJSON(t, engine, http.MethodPost, "/skills/"+one.ID+"/scope", `{"scope":"global"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("per-row flip of an imported skill = %d, want 400 (%s)", w.Code, w.Body.String())
	}

	// The group flips together, authors kept.
	body := `{"repo":"https://github.com/o/r","scope":"global"}`
	if w := doJSON(t, engine, http.MethodPost, "/skill-repos/scope", body); w.Code != http.StatusNoContent {
		t.Fatalf("promote repo = %d (%s)", w.Code, w.Body.String())
	}
	rows, err := st.ListMeta(t.Context(), store.LocalUserID, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, sk := range rows {
		if sk.Scope != store.ScopeGlobal || sk.OwnerID != store.LocalUserID {
			t.Fatalf("after promote %s = (%s, %s), want global and its author", sk.Name, sk.Scope, sk.OwnerID)
		}
	}
	// A second promote is a no-op the caller should hear about.
	if w := doJSON(t, engine, http.MethodPost, "/skill-repos/scope", body); w.Code != http.StatusConflict {
		t.Fatalf("re-promote = %d, want 409", w.Code)
	}

	// A sync's new file joins the published group instead of landing private.
	files["xlsx/SKILL.md"] = "---\nname: xlsx\ndescription: Spreadsheets.\n---\nBody.\n"
	gh2 := fakeGitHub(t, "sha2", files)
	h.githubAPI, h.githubRaw = gh2.URL, gh2.URL
	if resp := importGitHub(t, engine); len(resp.Created) != 1 || resp.Created[0] != "xlsx" {
		t.Fatalf("sync = %+v", resp)
	}
	fresh, err := st.GetByNameFor(t.Context(), "o/r:xlsx", store.LocalUserID)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Scope != store.ScopeGlobal {
		t.Fatalf("a sync's new file = %s, want the group's global scope", fresh.Scope)
	}

	// Unpublishing returns the whole group to its author.
	if w := doJSON(t, engine, http.MethodPost, "/skill-repos/scope", `{"repo":"https://github.com/o/r","scope":"private"}`); w.Code != http.StatusNoContent {
		t.Fatalf("demote repo = %d (%s)", w.Code, w.Body.String())
	}
	rows, _ = st.ListMeta(t.Context(), store.LocalUserID, false)
	for _, sk := range rows {
		if sk.Scope != store.ScopePrivate || sk.OwnerID != store.LocalUserID {
			t.Fatalf("after demote %s = (%s, %s), want the author's private set", sk.Name, sk.Scope, sk.OwnerID)
		}
	}
}

// A sync names the GROUP it refreshes, not just the repo. With two groups for
// one repository — a member's published one and the admin's own private copy
// — syncing the published one must update THAT group, never quietly refresh
// the caller's instead (decisions §5.31).
func TestSkillSyncTargetsTheNamedGroup(t *testing.T) {
	engine, h, st := skillTestEnv(t)
	engine.POST("/skill-repos/scope", h.SetRepoScope)
	files := map[string]string{"pdf/SKILL.md": pdfSkillDoc}
	gh := fakeGitHub(t, "sha1", files)
	h.githubAPI, h.githubRaw = gh.URL, gh.URL

	// The caller (an admin, in this rig) holds their own private group.
	if resp := importGitHub(t, engine); len(resp.Created) != 1 {
		t.Fatalf("import = %+v", resp)
	}
	// A second, foreign group for the same repository, one version behind:
	// PUBLISHED, so an admin may sync it (a member's private group is not an
	// admin's to write — asserted below).
	const foreign = "u-foreign"
	theirs := &store.Skill{Name: "pdf-processing", Description: "old", Content: "OLD BODY",
		Scope: store.ScopeGlobal, OwnerID: foreign,
		SourceRepo: "https://github.com/o/r", SourcePath: "pdf/SKILL.md", SourceSHA: "sha0"}
	if err := st.Create(t.Context(), theirs); err != nil {
		t.Fatal(err)
	}

	// Syncing the FOREIGN group updates it, and leaves the caller's alone.
	files["pdf/SKILL.md"] = strings.Replace(pdfSkillDoc, "Step 1.", "Step 2.", 1)
	gh2 := fakeGitHub(t, "sha2", files)
	h.githubAPI, h.githubRaw = gh2.URL, gh2.URL
	w := doJSON(t, engine, http.MethodPost, "/skill-imports",
		`{"url":"https://github.com/o/r","owner_id":"`+foreign+`"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("sync of the foreign group = %d (%s)", w.Code, w.Body.String())
	}
	got, err := st.Get(t.Context(), theirs.ID)
	if err != nil || !strings.Contains(got.Content, "Step 2.") {
		t.Fatalf("the named group was not refreshed: %+v (%v)", got, err)
	}
	var mine *store.Skill
	for _, sk := range listOwnSkills(t, st, store.LocalUserID) {
		if sk.SourcePath == "pdf/SKILL.md" {
			full, err := st.Get(t.Context(), sk.ID)
			if err != nil {
				t.Fatal(err)
			}
			mine = full
		}
	}
	if mine == nil {
		t.Fatal("the caller's own row vanished")
	}
	if strings.Contains(mine.Content, "Step 2.") {
		t.Fatal("syncing another group refreshed the caller's own rows")
	}

	// A group nobody holds is a 404, not a fresh group under their name.
	w = doJSON(t, engine, http.MethodPost, "/skill-imports",
		`{"url":"https://github.com/o/r","owner_id":"u-nobody"}`)
	if w.Code != http.StatusNotFound {
		t.Fatalf("sync of a group that does not exist = %d, want 404", w.Code)
	}

	// A member's PRIVATE group is not an admin's to write: management is
	// delete and scope change, not authorship (decisions §5.29). It reads as
	// absent, exactly as the row does elsewhere.
	const shy = "u-shy"
	private := &store.Skill{Name: "pdf-processing", Description: "theirs", Content: "PRIVATE",
		Scope: store.ScopePrivate, OwnerID: shy,
		SourceRepo: "https://github.com/o/r", SourcePath: "other/SKILL.md", SourceSHA: "sha0"}
	if err := st.Create(t.Context(), private); err != nil {
		t.Fatal(err)
	}
	w = doJSON(t, engine, http.MethodPost, "/skill-imports",
		`{"url":"https://github.com/o/r","owner_id":"`+shy+`"}`)
	if w.Code != http.StatusNotFound {
		t.Fatalf("admin syncing a member's private group = %d, want 404", w.Code)
	}
	if got, _ := st.Get(t.Context(), private.ID); got.Content != "PRIVATE" {
		t.Fatalf("the member's private row was written: %q", got.Content)
	}
}

// listOwnSkills is the rows ownerID authored, from the visible listing.
func listOwnSkills(t *testing.T, st *store.SkillStore, ownerID string) []store.Skill {
	t.Helper()
	rows, err := st.ListMeta(t.Context(), ownerID, false)
	if err != nil {
		t.Fatal(err)
	}
	var own []store.Skill
	for _, r := range rows {
		if r.OwnerID == ownerID {
			own = append(own, r)
		}
	}
	return own
}
