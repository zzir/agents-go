package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/zzir/agents-go/cmd/agents-server/internal/protocol"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
	"github.com/zzir/agents-go/skills"
)

// Skill imports: walking a GitHub repository or fetching one raw SKILL.md,
// anonymously and pinned to one commit (spec §5.26).

// maxImportSkills caps how many SKILL.md files one import walks, so a huge
// repo cannot flood the table in one request.
const maxImportSkills = 200

// errSkillDetached aborts an import's update inside the transaction when the
// row detached between the read and the write.
var errSkillDetached = errors.New("edited locally (detached)")

type skillImportReq struct {
	URL string `json:"url" binding:"required"`
}

// skillImportResp reports what one import did, per SKILL.md it saw. Skipped
// entries carry their reason ("path: why"), so nothing is dropped silently.
type skillImportResp struct {
	Repo      string   `json:"repo"`
	Created   []string `json:"created,omitempty"`
	Updated   []string `json:"updated,omitempty"`
	Unchanged []string `json:"unchanged,omitempty"`
	Skipped   []string `json:"skipped,omitempty"`
	// Truncated reports that GitHub's tree listing was cut off (a very large
	// repo): everything listed was imported, but files past the cut were not
	// seen at all.
	Truncated bool `json:"truncated,omitempty"`
}

// Import fetches SKILL.md documents from a URL and upserts them: a GitHub
// repository URL is traversed via the GitHub API (every SKILL.md at any
// depth), any other http(s) URL is fetched as one raw SKILL.md. Re-importing
// the same source refreshes rows that were not edited locally.
//
//	@Summary		Import skills from a URL
//	@Description	https://github.com/owner/repo imports every SKILL.md in the repo (GitHub API; set the github_token setting for private repos and rate limits). Any other http(s) URL is fetched as a single SKILL.md.
//	@Tags			skills
//	@Accept			json
//	@Produce		json
//	@Param			import	body		skillImportReq	true	"Repository or raw SKILL.md URL"
//	@Success		200		{object}	skillImportResp
//	@Failure		400		{object}	ErrorResponse
//	@Failure		502		{object}	ErrorResponse	"fetch failed"
//	@Security		BearerAuth
//	@Router			/skill-imports [post]
func (h *SkillHandler) Import(c *gin.Context) {
	var req skillImportReq
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, "url is required")
		return
	}
	rawURL := strings.TrimSpace(req.URL)
	lower := strings.ToLower(rawURL)
	if !strings.HasPrefix(lower, "https://") && !strings.HasPrefix(lower, "http://") {
		badRequest(c, "only http(s) URLs are supported")
		return
	}
	var resp *skillImportResp
	var err error
	if owner, repo, ok := parseGitHubRepoURL(rawURL); ok {
		resp, err = h.importGitHubRepo(c, owner, repo)
	} else {
		resp, err = h.importRawURL(c, rawURL)
	}
	if err != nil {
		abortError(c, http.StatusBadGateway, protocol.CodeUpstream, err.Error())
		return
	}
	if resp == nil {
		return // the importer already answered (a 4xx)
	}
	c.JSON(http.StatusOK, resp)
}

// parseGitHubRepoURL recognizes a repository home URL —
// https://github.com/{owner}/{repo}, optional .git or trailing slash. Deeper
// paths (a file, a tree) are not repo imports and fall through to the raw
// fetch.
func parseGitHubRepoURL(raw string) (owner, repo string, ok bool) {
	u, err := url.Parse(raw)
	if err != nil || !strings.EqualFold(u.Host, "github.com") {
		return "", "", false
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], strings.TrimSuffix(parts[1], ".git"), true
}

// httpClient is the import fetcher: the proxy_url client when one is set,
// the default client otherwise (ProxyClient returns nil for "no proxy").
func (h *SkillHandler) httpClient(ctx context.Context) *http.Client {
	if c := h.settings.ProxyClient(ctx); c != nil {
		return c
	}
	return http.DefaultClient
}

// githubGet performs one anonymous GitHub API GET (two calls per import, so
// the anonymous rate limit goes far).
func (h *SkillHandler) githubGet(c *gin.Context, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(c.Request.Context(), http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "agents-server")
	req.Header.Set("Accept", "application/vnd.github+json")
	return h.httpClient(c.Request.Context()).Do(req)
}

// readBody drains a response up to maxSkillBytes, reporting oversize as an
// explicit error rather than a truncated document.
func readBody(resp *http.Response) ([]byte, error) {
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxSkillBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxSkillBytes {
		return nil, fmt.Errorf("larger than %d KiB", maxSkillBytes>>10)
	}
	return data, nil
}

// importGitHubRepo walks a repository pinned at its HEAD commit: one call for
// the commit sha, one for the full tree, then a raw fetch per SKILL.md — all
// at the same commit, so a push mid-import cannot mix versions.
func (h *SkillHandler) importGitHubRepo(c *gin.Context, owner, repo string) (*skillImportResp, error) {
	api := h.githubAPI + "/repos/" + owner + "/" + repo
	head, err := h.githubGet(c, api+"/commits/HEAD")
	if err != nil {
		return nil, fmt.Errorf("github: %w", err)
	}
	if head.StatusCode == http.StatusNotFound {
		_ = head.Body.Close()
		notFound(c)
		return nil, nil
	}
	if head.StatusCode != http.StatusOK {
		_ = head.Body.Close()
		return nil, fmt.Errorf("github: HEAD lookup answered %s", head.Status)
	}
	var commit struct {
		SHA string `json:"sha"`
	}
	data, err := readBody(head)
	if err == nil {
		err = json.Unmarshal(data, &commit)
	}
	if err != nil {
		return nil, fmt.Errorf("github: reading HEAD commit: %w", err)
	}
	if commit.SHA == "" {
		return nil, fmt.Errorf("github: HEAD commit response carries no sha")
	}

	tree, err := h.githubGet(c, api+"/git/trees/"+commit.SHA+"?recursive=1")
	if err != nil {
		return nil, fmt.Errorf("github: %w", err)
	}
	if tree.StatusCode != http.StatusOK {
		_ = tree.Body.Close()
		return nil, fmt.Errorf("github: tree listing answered %s", tree.Status)
	}
	var listing struct {
		Tree []struct {
			Path string `json:"path"`
			Type string `json:"type"`
		} `json:"tree"`
		Truncated bool `json:"truncated"`
	}
	defer func() { _ = tree.Body.Close() }()
	if err := json.NewDecoder(tree.Body).Decode(&listing); err != nil {
		return nil, fmt.Errorf("github: reading tree: %w", err)
	}

	repoURL := "https://github.com/" + owner + "/" + repo
	resp := &skillImportResp{Repo: repoURL, Truncated: listing.Truncated}
	seen := 0
	for _, e := range listing.Tree {
		if e.Type != "blob" || (e.Path != "SKILL.md" && !strings.HasSuffix(e.Path, "/SKILL.md")) {
			continue
		}
		if seen++; seen > maxImportSkills {
			resp.Skipped = append(resp.Skipped, e.Path+": over the per-import cap of "+fmt.Sprint(maxImportSkills))
			continue
		}
		raw, err := h.githubGet(c, h.githubRaw+"/"+owner+"/"+repo+"/"+commit.SHA+"/"+e.Path)
		if err != nil {
			resp.Skipped = append(resp.Skipped, e.Path+": "+err.Error())
			continue
		}
		if raw.StatusCode != http.StatusOK {
			status := raw.Status
			_ = raw.Body.Close()
			resp.Skipped = append(resp.Skipped, e.Path+": fetch answered "+status)
			continue
		}
		content, err := readBody(raw)
		if err != nil {
			resp.Skipped = append(resp.Skipped, e.Path+": "+err.Error())
			continue
		}
		h.upsertImported(c, repoURL, e.Path, commit.SHA, content, resp)
	}
	if seen == 0 {
		badRequest(c, "repository contains no SKILL.md")
		return nil, nil
	}
	return resp, nil
}

// importRawURL fetches one SKILL.md from any http(s) URL — the escape hatch
// for skills hosted outside GitHub. No stored token rides along.
func (h *SkillHandler) importRawURL(c *gin.Context, rawURL string) (*skillImportResp, error) {
	req, err := http.NewRequestWithContext(c.Request.Context(), http.MethodGet, rawURL, nil)
	if err != nil {
		badRequest(c, "invalid url: "+err.Error())
		return nil, nil //nolint:nilerr // the 400 above is the answer; nil,nil = "already responded"
	}
	req.Header.Set("User-Agent", "agents-server")
	raw, err := h.httpClient(c.Request.Context()).Do(req)
	if err != nil {
		return nil, err
	}
	if raw.StatusCode != http.StatusOK {
		_ = raw.Body.Close()
		return nil, fmt.Errorf("fetch answered %s", raw.Status)
	}
	content, err := readBody(raw)
	if err != nil {
		return nil, err
	}
	resp := &skillImportResp{Repo: rawURL}
	h.upsertImported(c, rawURL, "", "", content, resp)
	return resp, nil
}

// upsertImported lands one fetched SKILL.md: matched by (source repo, path),
// created when new, refreshed when unedited, skipped when detached — a local
// edit is never overwritten by an import.
func (h *SkillHandler) upsertImported(c *gin.Context, repo, path, sha string, content []byte, resp *skillImportResp) {
	label := path
	if label == "" {
		label = repo
	}
	meta, err := skills.Parse(content)
	if err != nil {
		resp.Skipped = append(resp.Skipped, label+": "+err.Error())
		return
	}
	ctx := c.Request.Context()
	ownerID, admin, ok := callerScope(c)
	if !ok {
		return
	}
	prev, err := h.store.FindBySource(ctx, repo, path, ownerID, admin)
	if err != nil {
		resp.Skipped = append(resp.Skipped, label+": "+err.Error())
		return
	}
	switch {
	case prev == nil:
		sk := &store.Skill{
			Name: meta.Name, Description: meta.Description, Content: string(content),
			Scope: store.ScopePrivate, OwnerID: ownerID,
			SourceRepo: repo, SourcePath: path, SourceSHA: sha,
		}
		if err := h.store.Create(ctx, sk); err != nil {
			if _, dup := store.UniqueViolation(err); dup {
				resp.Skipped = append(resp.Skipped, label+": name "+meta.Name+" already in use")
			} else {
				resp.Skipped = append(resp.Skipped, label+": "+err.Error())
			}
			return
		}
		resp.Created = append(resp.Created, meta.Name)
	case prev.Detached:
		resp.Skipped = append(resp.Skipped, label+": edited locally (detached)")
	case prev.Content == string(content):
		resp.Unchanged = append(resp.Unchanged, meta.Name)
	default:
		sk := &store.Skill{
			Name: meta.Name, Description: meta.Description, Content: string(content),
			SourceRepo: repo, SourcePath: path, SourceSHA: sha,
		}
		err := h.store.Update(ctx, prev.ID, sk, func(p *store.Skill) error {
			if p.Detached {
				return errSkillDetached // detached between the read and the write
			}
			sk.Scope, sk.OwnerID = p.Scope, p.OwnerID
			return nil
		})
		if err != nil {
			switch _, dup := store.UniqueViolation(err); {
			case dup:
				resp.Skipped = append(resp.Skipped, label+": name "+meta.Name+" already in use")
			case errors.Is(err, errSkillDetached):
				resp.Skipped = append(resp.Skipped, label+": edited locally (detached)")
			default:
				resp.Skipped = append(resp.Skipped, label+": "+err.Error())
			}
			return
		}
		resp.Updated = append(resp.Updated, meta.Name)
	}
}
