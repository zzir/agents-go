package handler

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/zzir/agents-go/cmd/agents-server/internal/protocol"
	"github.com/zzir/agents-go/cmd/agents-server/internal/server"
	"github.com/zzir/agents-go/cmd/agents-server/internal/settings"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
	"github.com/zzir/agents-go/cmd/agents-server/internal/testdb"
)

func newSettingEngine(t *testing.T) (*gin.Engine, *store.SettingStore) {
	t.Helper()
	return newSettingEngineAs(t, protocol.UserInfo{ID: store.LocalUserID, Email: "local@localhost", Role: store.RoleAdmin})
}

// newSettingEngineAs mounts the setting routes with user signed in.
func newSettingEngineAs(t *testing.T, user protocol.UserInfo) (*gin.Engine, *store.SettingStore) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	st := store.NewSettingStore(testdb.New(t))
	h := NewSettingHandler(st)
	e := gin.New()
	e.Use(func(c *gin.Context) { server.SetCurrentUser(c, user); c.Next() })
	e.GET("/settings", h.List)
	e.GET("/settings/:key", h.Get)
	e.PUT("/settings/:key", h.Set)
	e.DELETE("/settings/:key", h.Delete)
	e.GET("/setting-defs", SettingDefList)
	return e, st
}

// A key the registry does not name is refused, so a typo cannot become a row
// that is stored forever and read by nobody.
func TestSetRejectsUnknownKey(t *testing.T) {
	e, st := newSettingEngine(t)
	if w := doJSON(t, e, http.MethodPut, "/settings/system_promptt", `{"value":"be terse"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", w.Code, w.Body)
	}
	if _, err := st.Get(t.Context(), "system_promptt"); err == nil {
		t.Fatal("a refused key must not have been stored")
	}
}

// The value has to suit the kind. Before this, "abc" for a number was stored
// and then silently ignored at read time.
func TestSetRejectsMalformedValues(t *testing.T) {
	e, _ := newSettingEngine(t)
	for _, tc := range []struct{ name, key, body string }{
		{"int gets words", settings.KeyTraceSpanDataKB, `{"value":"lots"}`},
		{"int below min", settings.KeyTraceSpanDataKB, `{"value":"0"}`},
		{"int above max", settings.KeyMaxTerminalsPerSandbox, `{"value":"500"}`},
		{"bool gets maybe", settings.KeyTraceIncludeSensitiveData, `{"value":"maybe"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if w := doJSON(t, e, http.MethodPut, "/settings/"+tc.key, tc.body); w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", w.Code, w.Body)
			}
		})
	}
}

func TestSetAcceptsValidValues(t *testing.T) {
	e, st := newSettingEngine(t)
	for _, tc := range []struct{ key, value string }{
		{settings.KeySystemPrompt, "be terse"},
		{settings.KeyTraceSpanDataKB, "4096"},
		{settings.KeyApprovalTTLMinutes, "0"},
		{settings.KeyTraceIncludeSensitiveData, "false"},
		// Empty is how a setting is returned to its default; never a 400.
		{settings.KeyTraceRetentionDays, ""},
	} {
		w := doJSON(t, e, http.MethodPut, "/settings/"+tc.key, `{"value":`+mustQuote(tc.value)+`}`)
		if w.Code != http.StatusOK {
			t.Fatalf("%s = %d, want 200: %s", tc.key, w.Code, w.Body)
		}
		got, err := st.Get(t.Context(), tc.key)
		if err != nil {
			t.Fatalf("%s: %v", tc.key, err)
		}
		if got.Value != tc.value {
			t.Errorf("%s stored %q, want %q", tc.key, got.Value, tc.value)
		}
	}
}

// A row the registry no longer names is still listed — and deletable. Hiding
// it would leave a value nobody can see or clear.
func TestListFlagsUnknownKeysAndDeleteClearsThem(t *testing.T) {
	e, st := newSettingEngine(t)
	if err := st.Set(t.Context(), "retired_key", "leftover"); err != nil {
		t.Fatal(err)
	}
	if err := st.Set(t.Context(), settings.KeySystemPrompt, "be terse"); err != nil {
		t.Fatal(err)
	}
	var list []SettingView
	if err := json.Unmarshal(doJSON(t, e, http.MethodGet, "/settings", "").Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	byKey := map[string]SettingView{}
	for _, v := range list {
		byKey[v.Key] = v
	}
	if !byKey["retired_key"].Unknown {
		t.Error("a key the registry dropped must be flagged, not hidden")
	}
	// Whether the retired key WAS a secret is unknowable once its def is gone.
	if byKey["retired_key"].Value != SecretMask {
		t.Errorf("an unknown row's value must be masked, got %q", byKey["retired_key"].Value)
	}
	if byKey[settings.KeySystemPrompt].Unknown {
		t.Error("a defined key must not be flagged unknown")
	}
	if w := doJSON(t, e, http.MethodDelete, "/settings/retired_key", ""); w.Code != http.StatusNoContent {
		t.Fatalf("delete = %d, want 204: %s", w.Code, w.Body)
	}
}

// The panel renders from this, so it must carry what a control needs.
func TestSettingDefsAreServed(t *testing.T) {
	e, _ := newSettingEngine(t)
	var defs []settings.Def
	if err := json.Unmarshal(doJSON(t, e, http.MethodGet, "/setting-defs", "").Body.Bytes(), &defs); err != nil {
		t.Fatal(err)
	}
	if len(defs) != len(settings.Defs()) {
		t.Fatalf("served %d defs, registry has %d", len(defs), len(settings.Defs()))
	}
	for _, d := range defs {
		if d.Key == "" || d.Kind == "" || d.Label == "" || d.Group == "" {
			t.Errorf("def %+v lost a field the panel needs", d)
		}
	}
}

func mustQuote(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// The storage group is the admin's to read as it is to write: a member's
// listing leaves the s3_* keys out and a direct read is 403.
func TestStorageSettingsAreReadByAdminsOnly(t *testing.T) {
	member := protocol.UserInfo{ID: "u-member", Email: "member@example.com", Role: store.RoleMember}
	e, st := newSettingEngineAs(t, member)
	for k, v := range map[string]string{settings.KeyS3Bucket: "pics", settings.KeyS3AccessKeyID: "AKIA", settings.KeySystemPrompt: "be terse"} {
		if err := st.Set(t.Context(), k, v); err != nil {
			t.Fatal(err)
		}
	}
	w := doJSON(t, e, http.MethodGet, "/settings", "")
	if strings.Contains(w.Body.String(), "s3_") || !strings.Contains(w.Body.String(), "system_prompt") {
		t.Fatalf("member listing = %s, want the storage keys out and the rest in", w.Body)
	}
	if w := doJSON(t, e, http.MethodGet, "/settings/s3_bucket", ""); w.Code != http.StatusForbidden {
		t.Fatalf("member GET s3_bucket = %d, want 403", w.Code)
	}
	ae, ast := newSettingEngineAs(t, protocol.UserInfo{ID: "u-admin", Role: store.RoleAdmin})
	if err := ast.Set(t.Context(), settings.KeyS3Bucket, "pics"); err != nil {
		t.Fatal(err)
	}
	if w := doJSON(t, ae, http.MethodGet, "/settings/s3_bucket", ""); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "pics") {
		t.Fatalf("admin GET s3_bucket = %d %s, want the value", w.Code, w.Body)
	}
	if w := doJSON(t, ae, http.MethodGet, "/settings", ""); !strings.Contains(w.Body.String(), "s3_bucket") {
		t.Fatalf("admin listing = %s, want the storage keys in", w.Body)
	}
}
