package handler

import (
	"bufio"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/gin-gonic/gin"
)

// SkillHandler manages skill directories, supporting listing, cloning, updating, and deletion.
type SkillHandler struct {
	skillsDir string
}

// NewSkillHandler returns a handler that stores skills under a "skills" directory within workspace.
func NewSkillHandler(workspace string) *SkillHandler {
	dir := filepath.Join(workspace, "skills")
	_ = os.MkdirAll(dir, 0o755) // best-effort; skill ops surface errors on use
	return &SkillHandler{skillsDir: dir}
}

type skillEntry struct {
	Name        string `json:"name"`
	Path        string `json:"path"`
	Description string `json:"description"`
}

// List responds with all discovered skills under the root directory.
//
//	@Summary	List skills
//	@Tags		skills
//	@Produce	json
//	@Success	200	{array}	skillEntry
//	@Security	BearerAuth
//	@Router		/skills [get]
func (h *SkillHandler) List(c *gin.Context) {
	skills := findAllSkills(h.skillsDir)
	c.JSON(http.StatusOK, skills)
}

// Get responds with the SKILL.md contents for the skill at the requested path.
//
//	@Summary	Get skill content
//	@Tags		skills
//	@Produce	json
//	@Param		path	path		string	true	"Skill path (may be nested, e.g. repo/sub-skill)"
//	@Success	200		{object}	skillContentResp
//	@Failure	400		{object}	ErrorResponse
//	@Failure	404		{object}	ErrorResponse
//	@Security	BearerAuth
//	@Router		/skills/{path} [get]
func (h *SkillHandler) Get(c *gin.Context) {
	relPath := c.Param("path")
	relPath = strings.TrimPrefix(relPath, "/")
	clean := filepath.Clean(relPath)
	if strings.Contains(clean, "..") || filepath.IsAbs(clean) {
		badRequest(c, "invalid skill path")
		return
	}
	resolved := filepath.Join(h.skillsDir, clean, "SKILL.md")
	if !strings.HasPrefix(resolved, h.skillsDir) {
		badRequest(c, "invalid skill path")
		return
	}
	data, err := os.ReadFile(resolved)
	if err != nil {
		notFound(c)
		return
	}
	c.JSON(http.StatusOK, skillContentResp{
		Name:    filepath.Base(clean),
		Path:    clean,
		Content: string(data),
	})
}

type cloneRequest struct {
	URL string `json:"url" binding:"required"`
}

// skillRepoResp is the Clone/Sync response: the repo name and the skills
// discovered inside it.
type skillRepoResp struct {
	Name   string       `json:"name"`
	Skills []skillEntry `json:"skills"`
}

// skillContentResp is the Get response: one skill's SKILL.md contents.
type skillContentResp struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	Content string `json:"content"`
}

// Clone shallow-clones a git repository of skills into the root directory.
// It responds with 201 and the discovered skills.
//
//	@Summary		Clone skill repo
//	@Description	git clone --depth=1 of an http(s) repository containing SKILL.md files.
//	@Tags			skill-repos
//	@Accept			json
//	@Produce		json
//	@Param			repo	body		cloneRequest	true	"Repository URL (http/https only)"
//	@Success		201		{object}	skillRepoResp
//	@Failure		400		{object}	ErrorResponse
//	@Failure		409		{object}	ErrorResponse	"directory already exists"
//	@Failure		502		{object}	ErrorResponse	"git clone failed"
//	@Security		BearerAuth
//	@Router			/skill-repos [post]
func (h *SkillHandler) Clone(c *gin.Context) {
	var req cloneRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, "url is required")
		return
	}

	repoURL := strings.TrimSpace(req.URL)
	if repoURL == "" {
		badRequest(c, "url is required")
		return
	}
	// Only plain http(s) remotes: rejects file://, ssh, and git's ext::
	// command transport, and (starting with a scheme) can't be mistaken for a
	// git flag.
	lower := strings.ToLower(repoURL)
	if !strings.HasPrefix(lower, "https://") && !strings.HasPrefix(lower, "http://") {
		badRequest(c, "only http(s) repository URLs are supported")
		return
	}

	name := repoNameFromURL(repoURL)
	if name == "" {
		badRequest(c, "cannot determine repo name from url")
		return
	}

	dest := filepath.Join(h.skillsDir, name)
	if _, err := os.Stat(dest); err == nil {
		conflict(c, "directory already exists: "+name)
		return
	}

	cmd := exec.CommandContext(c.Request.Context(), "git", "clone", "--depth=1", "--", repoURL, dest)
	output, err := cmd.CombinedOutput()
	if err != nil {
		_ = os.RemoveAll(dest)
		abortError(c, http.StatusBadGateway, CodeUpstream, "git clone failed: "+string(output))
		return
	}

	skills := findAllSkills(dest)
	if len(skills) == 0 {
		_ = os.RemoveAll(dest)
		badRequest(c, "cloned repo does not contain any SKILL.md")
		return
	}

	c.JSON(http.StatusCreated, skillRepoResp{Name: name, Skills: skills})
}

// repoDir validates the repo name path parameter and resolves it inside the
// skills directory. It reports the failure to c and returns "" when invalid.
func (h *SkillHandler) repoDir(c *gin.Context) string {
	name := c.Param("name")
	clean := filepath.Clean(name)
	if strings.Contains(clean, "..") || filepath.IsAbs(clean) {
		badRequest(c, "invalid skill repo name")
		return ""
	}
	target := filepath.Join(h.skillsDir, clean)
	if !strings.HasPrefix(target, h.skillsDir) {
		badRequest(c, "invalid skill repo name")
		return ""
	}
	return target
}

// Delete removes the skill repo directory identified by the name path parameter.
//
//	@Summary	Delete skill repo
//	@Tags		skill-repos
//	@Param		name	path	string	true	"Repo directory name"
//	@Success	204		"deleted"
//	@Failure	400		{object}	ErrorResponse
//	@Failure	404		{object}	ErrorResponse
//	@Failure	500		{object}	ErrorResponse
//	@Security	BearerAuth
//	@Router		/skill-repos/{name} [delete]
func (h *SkillHandler) Delete(c *gin.Context) {
	target := h.repoDir(c)
	if target == "" {
		return
	}
	if _, err := os.Stat(target); err != nil {
		notFound(c)
		return
	}
	if err := os.RemoveAll(target); err != nil {
		internalError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// Sync fetches and hard-resets the skill git repository identified by the
// name path parameter, then responds with the refreshed skill list.
//
//	@Summary		Sync skill repo
//	@Description	git fetch + reset --hard origin/HEAD; local changes in the repo are discarded.
//	@Tags			skill-repos
//	@Produce		json
//	@Param			name	path		string	true	"Repo directory name"
//	@Success		200		{object}	skillRepoResp
//	@Failure		400		{object}	ErrorResponse
//	@Failure		404		{object}	ErrorResponse
//	@Failure		409		{object}	ErrorResponse	"not a git repository"
//	@Failure		502		{object}	ErrorResponse	"git fetch failed"
//	@Security		BearerAuth
//	@Router			/skill-repos/{name}/sync [post]
func (h *SkillHandler) Sync(c *gin.Context) {
	target := h.repoDir(c)
	if target == "" {
		return
	}
	if _, err := os.Stat(target); err != nil {
		notFound(c)
		return
	}
	if _, err := os.Stat(filepath.Join(target, ".git")); err != nil {
		conflict(c, "not a git repository")
		return
	}

	ctx := c.Request.Context()
	fetch := exec.CommandContext(ctx, "git", "-C", target, "fetch", "--depth=1")
	if out, err := fetch.CombinedOutput(); err != nil {
		abortError(c, http.StatusBadGateway, CodeUpstream, "git fetch failed: "+string(out))
		return
	}
	reset := exec.CommandContext(ctx, "git", "-C", target, "reset", "--hard", "origin/HEAD")
	if out, err := reset.CombinedOutput(); err != nil {
		internalError(c, errors.New("git reset failed: "+string(out)))
		return
	}

	skills := findAllSkills(target)
	c.JSON(http.StatusOK, skillRepoResp{Name: c.Param("name"), Skills: skills})
}

func findAllSkills(root string) []skillEntry {
	var skills []skillEntry
	// WalkDir only errors if the callback does; ours never returns a non-nil
	// error (unreadable entries are skipped), so the return is always nil.
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // skip unreadable entries, keep walking
		}
		if d.IsDir() && (d.Name() == ".git" || d.Name() == "node_modules") {
			return filepath.SkipDir
		}
		if !d.IsDir() && d.Name() == "SKILL.md" {
			dir := filepath.Dir(path)
			rel, _ := filepath.Rel(root, dir)
			if rel == "." {
				rel = filepath.Base(root)
			}
			skills = append(skills, skillEntry{
				Name:        filepath.Base(dir),
				Path:        rel,
				Description: extractDescription(path),
			})
		}
		return nil
	})
	if skills == nil {
		skills = []skillEntry{}
	}
	return skills
}

func extractDescription(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	inFrontmatter := false
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		if line == "---" {
			if !inFrontmatter {
				inFrontmatter = true
				continue
			}
			// End of frontmatter — fall through to read first content line
			inFrontmatter = false
			continue
		}

		if inFrontmatter {
			if strings.HasPrefix(line, "description:") {
				desc := strings.TrimPrefix(line, "description:")
				desc = strings.TrimSpace(desc)
				desc = strings.Trim(desc, ">")
				desc = strings.TrimSpace(desc)
				if desc != "" {
					return desc
				}
				// Folded/multi-line description (e.g. "description: >"): return the
				// first non-blank continuation line, skipping blanks; stop at the
				// frontmatter end or the next "key:" line.
				for scanner.Scan() {
					next := strings.TrimSpace(scanner.Text())
					if next == "" {
						continue
					}
					if next == "---" || strings.Contains(next, ":") {
						break
					}
					return next
				}
			}
			continue
		}

		if line == "" {
			continue
		}
		// First non-empty line after frontmatter (or no frontmatter)
		return strings.TrimLeft(line, "# ")
	}
	return ""
}

func repoNameFromURL(url string) string {
	url = strings.TrimSuffix(url, ".git")
	url = strings.TrimRight(url, "/")
	parts := strings.Split(url, "/")
	if len(parts) == 0 {
		return ""
	}
	name := parts[len(parts)-1]
	name = filepath.Clean(name)
	if name == "" || name == "." || name == ".." {
		return ""
	}
	return name
}
