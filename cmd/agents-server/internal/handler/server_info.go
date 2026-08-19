package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// ServerInfo is the process-level configuration a client is subject to but
// cannot change: the flags this server was started with, resolved to what is
// actually in force. It exists because a UI that cannot see them can only
// report their effects as unexplained refusals — a "local sandboxes are not
// allowed" with nowhere to learn why.
type ServerInfo struct {
	Version string `json:"version"`
	// Workspace is absolute: the relative default (".") means nothing to a
	// browser on another machine.
	Workspace         string `json:"workspace"`
	AllowLocalSandbox bool   `json:"allow_local_sandbox"`
	// MaxTasks is the effective cap, never the raw flag — 0 on the command
	// line means the built-in default, and reporting the 0 would be a lie.
	MaxTasks int `json:"max_tasks"`
}

// ServerInfoHandler answers with info. Bound at registration rather than read
// from a store: these are process facts, fixed for its lifetime.
//
//	@Summary		Server info
//	@Description	The start-up configuration in force: version, workspace, and the flags a client cannot change. Read-only — these come from the command line, not the settings table.
//	@Tags			server
//	@Produce		json
//	@Success		200	{object}	ServerInfo
//	@Security		BearerAuth
//	@Router			/server [get]
func ServerInfoHandler(info ServerInfo) gin.HandlerFunc {
	return func(c *gin.Context) { c.JSON(http.StatusOK, info) }
}
