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
	"time"

	"github.com/gin-gonic/gin"

	"github.com/zzir/agents-go/cmd/agents-server/internal/protocol"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
	"github.com/zzir/agents-go/skills"
)

// Skill imports: walking a GitHub repository or fetching one raw SKILL.md,
// anonymously and pinned to one commit (decisions §5.26).

// maxImportSkills caps how many SKILL.md files one import walks, so a huge
// repo cannot flood the table in one request.
const maxImportSkills = 200

type skillImportReq struct {
	URL string `json:"url" binding:"required"`
	// OwnerID names WHICH group this import refreshes — a sync of somebody
	// else's published repository, for an admin. Empty means the caller's own
	// group, the only one a first import may create (decisions §5.31).
	OwnerID string `json:"owner_id,omitempty"`
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

// importTargetKey is where resolveImportTarget parks the group an import
// writes into, for upsertImported to read.
const importTargetKey = "agents.importTarget"

// importGroup is the group an import writes into, as resolved BEFORE the
// fetch: whose it is, and the shape the apply re-checks it still has.
type importGroup struct {
	owner   string
	scope   string
	existed bool
}

// resolveImportTarget decides WHOSE group this import refreshes and whether
// the caller may write it. Empty owner_id is the caller's own group — the
// only one a first import may create. Naming another owner targets their
// PUBLISHED group: an admin manages shared configuration but never edits a
// member's private rows (decisions §5.29), so a foreign private group answers 404
// exactly as it reads elsewhere, and so does one that does not exist. False
// means the response is written.
func (h *SkillHandler) resolveImportTarget(c *gin.Context, repo, wantOwner string) bool {
	ownerID, admin, ok := callerScope(c)
	if !ok {
		return false
	}
	ctx := c.Request.Context()
	if wantOwner == "" || wantOwner == ownerID {
		scope, existed, err := h.store.RepoGroup(ctx, repo, ownerID)
		if err != nil {
			internalError(c, err)
			return false
		}
		c.Set(importTargetKey, importGroup{owner: ownerID, scope: scope, existed: existed})
		return true
	}
	if !admin {
		abortError(c, http.StatusForbidden, protocol.CodeForbidden, "admin role required to sync another member's repository")
		return false
	}
	// The group is looked up by the URL AS GIVEN — the same string the rows
	// carry — so a raw-URL group is reachable too.
	scope, found, err := h.store.RepoGroup(ctx, repo, wantOwner)
	if err != nil {
		internalError(c, err)
		return false
	}
	if !found || scope != store.ScopeGlobal {
		notFound(c) // a member's private group is not an admin's to write
		return false
	}
	c.Set(importTargetKey, importGroup{owner: wantOwner, scope: scope, existed: true})
	return true
}

// importTarget is the group resolveImportTarget settled on.
func importTarget(c *gin.Context) importGroup {
	v, _ := c.Get(importTargetKey)
	g, _ := v.(importGroup)
	return g
}

// Import fetches SKILL.md documents from a URL and upserts them: a GitHub
// repository URL is traversed via the GitHub API (every SKILL.md at any
// depth), any other http(s) URL is fetched as one raw SKILL.md. Re-importing
// the same source refreshes rows that were not edited locally.
//
//	@Summary		Import skills from a URL
//	@Description	https://github.com/owner/repo imports every SKILL.md in the repo (anonymous GitHub API — private repositories are not reachable). Any other http(s) URL is fetched as a single SKILL.md.
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
	if !h.resolveImportTarget(c, rawURL, req.OwnerID) {
		return
	}
	// One deadline over the whole import: a GitHub walk is up to ~202 serial
	// fetches, and per-fetch timeouts alone would let a stalling target
	// stretch that into hours (decisions §5.26).
	ctx, cancel := context.WithTimeout(c.Request.Context(), skillImportBudget)
	defer cancel()
	c.Request = c.Request.WithContext(ctx)
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

// skillFetchTimeout bounds one import fetch, connect through body read: a
// target that accepts and stalls must not hold the handler's goroutine and
// connection open for as long as the client cares to wait.
const skillFetchTimeout = 30 * time.Second

// skillImportBudget bounds one whole import, fetches and writes together.
// Files past an expired budget land in skipped with the deadline error.
const skillImportBudget = 5 * time.Minute

// maxImportBytes bounds what one import holds in memory: the documents are
// all fetched before any is written, so the cap is on their sum, not just on
// each (maxSkillBytes).
const maxImportBytes = 16 << 20

// httpClient is the import fetcher: the proxy_url client when one is set,
// a plain client otherwise (ProxyClient returns nil for "no proxy") — either
// way bounded by skillFetchTimeout.
func (h *SkillHandler) httpClient(ctx context.Context) *http.Client {
	c := h.settings.ProxyClient(ctx)
	if c == nil {
		c = &http.Client{}
	}
	c.Timeout = skillFetchTimeout
	return c
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
	// Everything is fetched first and written in one transaction at the end
	// (apply), so the group is checked once, at an instant, rather than once
	// per file across minutes of downloading.
	var docs []store.ImportDoc
	seen, fetched := 0, 0
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
		if fetched += len(content); fetched > maxImportBytes {
			resp.Skipped = append(resp.Skipped, e.Path+": over the per-import size budget")
			break
		}
		collect(&docs, resp, e.Path, commit.SHA, content)
	}
	if seen == 0 {
		badRequest(c, "repository contains no SKILL.md")
		return nil, nil
	}
	if !h.apply(c, repoURL, docs, resp) {
		return nil, nil // the apply already answered
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
	var docs []store.ImportDoc
	collect(&docs, resp, "", "", content)
	if !h.apply(c, rawURL, docs, resp) {
		return nil, nil // the apply already answered
	}
	return resp, nil
}

// collect parses one fetched document and adds it to the batch the apply
// lands. A document that is not a valid SKILL.md is skipped here, before any
// transaction — a parse failure is the file's, not the group's.
func collect(docs *[]store.ImportDoc, resp *skillImportResp, path, sha string, content []byte) {
	label := path
	if label == "" {
		label = "the document"
	}
	meta, err := skills.Parse(content)
	if err != nil {
		resp.Skipped = append(resp.Skipped, label+": "+err.Error())
		return
	}
	*docs = append(*docs, store.ImportDoc{
		Path: path, SHA: sha,
		Name: meta.Name, Description: meta.Description, Content: string(content),
	})
}

// apply lands the fetched batch in ONE transaction against the group resolved
// before the fetch (decisions §5.31). Everything up to here was network I/O, up to
// the whole import budget; a transfer or scope flip that landed meanwhile
// refuses the apply outright (409) rather than writing part of it.
func (h *SkillHandler) apply(c *gin.Context, repo string, docs []store.ImportDoc, resp *skillImportResp) bool {
	g := importTarget(c)
	outcomes, err := h.store.ApplyImport(c.Request.Context(), repo, g.owner, g.scope, g.existed, docs)
	if err != nil {
		if errors.Is(err, store.ErrOwnershipChanged) {
			conflict(c, "the repository's skills moved or changed scope during the import; nothing was written — reload and sync again")
			return false
		}
		internalError(c, err)
		return false
	}
	for _, o := range outcomes {
		switch o.Action {
		case "created":
			resp.Created = append(resp.Created, o.Name)
		case "updated":
			resp.Updated = append(resp.Updated, o.Name)
		case "unchanged":
			resp.Unchanged = append(resp.Unchanged, o.Name)
		default:
			resp.Skipped = append(resp.Skipped, o.Label+": "+o.Reason)
		}
	}
	return true
}
