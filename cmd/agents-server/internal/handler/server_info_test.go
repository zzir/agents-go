package handler

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// The process facts a client is subject to are readable at /server.
func TestServerInfoReportsTheEffectiveConfig(t *testing.T) {
	gin.SetMode(gin.TestMode)
	e := newTestEngine()
	e.GET("/server", ServerInfoHandler(ServerInfo{Version: "1.2.3", Timezone: "Asia/Shanghai", CredentialsSealed: true}))
	body := doJSON(t, e, http.MethodGet, "/server", "").Body.Bytes()
	var got ServerInfo
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got.Version != "1.2.3" || got.Timezone != "Asia/Shanghai" || !got.CredentialsSealed {
		t.Fatalf("server info = %+v", got)
	}
	for _, key := range []string{`"timezone"`, `"credentials_sealed"`} {
		if !strings.Contains(string(body), key) {
			t.Fatalf("server info JSON lacks %s: %s", key, body)
		}
	}
}
