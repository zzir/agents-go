package handler

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
)

// The process facts a client is subject to are readable at /server.
func TestServerInfoReportsTheEffectiveConfig(t *testing.T) {
	gin.SetMode(gin.TestMode)
	e := newTestEngine()
	e.GET("/server", ServerInfoHandler(ServerInfo{Version: "1.2.3"}))
	var got ServerInfo
	if err := json.Unmarshal(doJSON(t, e, http.MethodGet, "/server", "").Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Version != "1.2.3" {
		t.Fatalf("server info = %+v", got)
	}
}
