package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/zzir/agents-go/cmd/agents-server/internal/providers"
)

// ProviderTypeList responds with the registered provider backends — machine
// facts only (auth modes, unsupported request features, global key setting).
// Display copy lives in the frontend; this endpoint exists so capability
// hints in the UI derive from the same declaration the build enforces,
// instead of drifting alongside it.
//
//	@Summary		List provider types
//	@Description	The backends agents, fallback entries and provider routes can select via provider_type. "unsupported" lists request features that fail loudly on that backend.
//	@Tags			provider-routes
//	@Produce		json
//	@Success		200	{array}	providers.TypeInfo
//	@Security		BearerAuth
//	@Router			/provider-types [get]
func ProviderTypeList(c *gin.Context) {
	c.JSON(http.StatusOK, providers.Types())
}
