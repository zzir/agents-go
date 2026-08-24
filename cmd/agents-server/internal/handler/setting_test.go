package handler

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/zzir/agents-go/cmd/agents-server/internal/settings"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
	"github.com/zzir/agents-go/cmd/agents-server/internal/testdb"
)

func newSettingEngine(t *testing.T) (*gin.Engine, *store.SettingStore) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	st := store.NewSettingStore(testdb.New(t))
	h := NewSettingHandler(st)
	e := newTestEngine()
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
	if w := doJSON(t, e, http.MethodPut, "/settings/proxy_urlll", `{"value":"http://127.0.0.1:1"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", w.Code, w.Body)
	}
	if _, err := st.Get(t.Context(), "proxy_urlll"); err == nil {
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
		{"proxy without a scheme", settings.KeyProxyURL, `{"value":"127.0.0.1:7890"}`},
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
		{settings.KeyProxyURL, "socks5://127.0.0.1:1080"},
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
	if err := st.Set(t.Context(), settings.KeyProxyURL, "http://127.0.0.1:7890"); err != nil {
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
	if byKey[settings.KeyProxyURL].Unknown {
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
