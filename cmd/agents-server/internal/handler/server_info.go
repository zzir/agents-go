package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// ServerInfo is the process-level configuration a client is subject to but
// cannot change — facts of the running process, not settings.
type ServerInfo struct {
	Version string `json:"version"`
	// Timezone is the zone cron schedules run in: the IANA name the process
	// started with, or "Local (abbr UTC±hh:mm)" when none was named.
	Timezone string `json:"timezone"`
	// CredentialsSealed reports whether stored credentials are encrypted at
	// rest (a secret key is configured).
	CredentialsSealed bool `json:"credentials_sealed"`
}

// ServerInfoHandler answers with info, fixed for the process's lifetime.
//
//	@Summary		Server info
//	@Description	The process facts a client is subject to but cannot change: version, the zone cron schedules run in, and whether stored credentials are sealed. Read-only — these come from the command line and environment, not the settings table.
//	@Tags			server
//	@Produce		json
//	@Success		200	{object}	ServerInfo
//	@Security		BearerAuth
//	@Router			/server [get]
func ServerInfoHandler(info ServerInfo) gin.HandlerFunc {
	return func(c *gin.Context) { c.JSON(http.StatusOK, info) }
}
