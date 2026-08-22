package handler

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
)

// The flags a client is subject to are readable, and max_tasks reports what is
// in force rather than the raw 0 the flag defaults to.
func TestServerInfoReportsTheEffectiveConfig(t *testing.T) {
	gin.SetMode(gin.TestMode)
	e := newTestEngine()
	e.GET("/server", ServerInfoHandler(ServerInfo{
		Version: "1.2.3", Workspace: "/srv/work", AllowLocalSandbox: false, MaxTasks: 6,
	}))
	var got ServerInfo
	if err := json.Unmarshal(doJSON(t, e, http.MethodGet, "/server", "").Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Version != "1.2.3" || got.Workspace != "/srv/work" || got.MaxTasks != 6 || got.AllowLocalSandbox {
		t.Fatalf("server info = %+v", got)
	}
}
