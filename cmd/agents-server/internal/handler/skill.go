package handler

import (
	"bufio"
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
	rootDir string
}

// NewSkillHandler returns a handler that stores skills under a "skills" directory within rootDir.
func NewSkillHandler(rootDir string) *SkillHandler {
	dir := filepath.Join(rootDir, "skills")
	_ = os.MkdirAll(dir, 0o755) // best-effort; skill ops surface errors on use
	return &SkillHandler{rootDir: dir}
}

type skillEntry struct {
	Name        string `json:"name"`
	Path        string `json:"path"`
	Description string `json:"description"`
}

// List responds with all discovered skills under the root directory.
func (h *SkillHandler) List(c *gin.Context) {
	skills := findAllSkills(h.rootDir)
	c.JSON(http.StatusOK, skills)
}

// Get responds with the SKILL.md contents for the skill at the requested path.
func (h *SkillHandler) Get(c *gin.Context) {
	relPath := c.Param("path")
	relPath = strings.TrimPrefix(relPath, "/")
	clean := filepath.Clean(relPath)
	if strings.Contains(clean, "..") || filepath.IsAbs(clean) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid skill path"})
		return
	}
	resolved := filepath.Join(h.rootDir, clean, "SKILL.md")
	if !strings.HasPrefix(resolved, h.rootDir) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid skill path"})
		return
	}
	data, err := os.ReadFile(resolved)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "skill not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"name":    filepath.Base(clean),
		"path":    clean,
		"content": string(data),
	})
}

type cloneRequest struct {
	URL string `json:"url" binding:"required"`
}

// Clone shallow-clones a git repository of skills into the root directory.
func (h *SkillHandler) Clone(c *gin.Context) {
	var req cloneRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "url is required"})
		return
	}

	repoURL := strings.TrimSpace(req.URL)
	if repoURL == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "url is required"})
		return
	}

	name := repoNameFromURL(repoURL)
	if name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "cannot determine repo name from url"})
		return
	}

	dest := filepath.Join(h.rootDir, name)
	if _, err := os.Stat(dest); err == nil {
		c.JSON(http.StatusConflict, gin.H{"error": "directory already exists: " + name})
		return
	}

	cmd := exec.CommandContext(c.Request.Context(), "git", "clone", "--depth=1", repoURL, dest)
	output, err := cmd.CombinedOutput()
	if err != nil {
		_ = os.RemoveAll(dest)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "git clone failed: " + string(output)})
		return
	}

	skills := findAllSkills(dest)
	if len(skills) == 0 {
		_ = os.RemoveAll(dest)
		c.JSON(http.StatusBadRequest, gin.H{"error": "cloned repo does not contain any SKILL.md"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"name": name, "skills": skills})
}

// Delete removes the skill directory identified by the name path parameter.
func (h *SkillHandler) Delete(c *gin.Context) {
	name := c.Param("name")
	clean := filepath.Clean(name)
	if strings.Contains(clean, "..") || filepath.IsAbs(clean) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid skill name"})
		return
	}
	target := filepath.Join(h.rootDir, clean)
	if !strings.HasPrefix(target, h.rootDir) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid skill name"})
		return
	}
	if _, err := os.Stat(target); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "skill not found"})
		return
	}
	if err := os.RemoveAll(target); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// Update fetches and resets the skill git repository identified by the name path parameter.
func (h *SkillHandler) Update(c *gin.Context) {
	name := c.Param("name")
	clean := filepath.Clean(name)
	if strings.Contains(clean, "..") || filepath.IsAbs(clean) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid skill name"})
		return
	}
	target := filepath.Join(h.rootDir, clean)
	if !strings.HasPrefix(target, h.rootDir) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid skill name"})
		return
	}
	if _, err := os.Stat(filepath.Join(target, ".git")); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "not a git repository"})
		return
	}

	ctx := c.Request.Context()
	fetch := exec.CommandContext(ctx, "git", "-C", target, "fetch", "--depth=1")
	if out, err := fetch.CombinedOutput(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "git fetch failed: " + string(out)})
		return
	}
	reset := exec.CommandContext(ctx, "git", "-C", target, "reset", "--hard", "origin/HEAD")
	if out, err := reset.CombinedOutput(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "git reset failed: " + string(out)})
		return
	}

	skills := findAllSkills(target)
	c.JSON(http.StatusOK, gin.H{"name": name, "skills": skills})
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
