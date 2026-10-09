package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/zzir/agents-go/cmd/agents-server/internal/protocol"
	"github.com/zzir/agents-go/cmd/agents-server/internal/providers"
	"github.com/zzir/agents-go/cmd/agents-server/internal/server"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
	"github.com/zzir/agents-go/cmd/agents-server/internal/testdb"
)

// fakeModelsAPI answers GET …/models like OpenAI does: a list for the good
// key, 401 for any other, counting the calls it served.
func fakeModelsAPI(t *testing.T, goodKey string, calls *atomic.Int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/models") {
			http.NotFound(w, r)
			return
		}
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("Authorization") != "Bearer "+goodKey {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"message":"Incorrect API key provided: ` + strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ") + `","type":"invalid_request_error"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"gpt-a","object":"model","created":0,"owned_by":"openai"},{"id":"gpt-b","object":"model","created":0,"owned_by":"openai"}]}`))
	}))
}

// providerEngine mounts the provider routes with the given caller signed in.
func providerEngine(h *ProviderHandler, user protocol.UserInfo) *gin.Engine {
	gin.SetMode(gin.TestMode)
	e := gin.New()
	e.Use(func(c *gin.Context) { server.SetCurrentUser(c, user); c.Next() })
	e.GET("/providers/:id/models", h.Models)
	e.POST("/providers/:id/test", h.Test)
	return e
}

func TestProviderModelsListsAndCaches(t *testing.T) {
	ctx := context.Background()
	db := testdb.New(t)
	var calls atomic.Int32
	api := fakeModelsAPI(t, "sk-good", &calls)
	defer api.Close()
	ps := store.NewProviderStore(db)
	pv := &store.Provider{OwnerID: store.LocalUserID, Name: "p", APIKey: "sk-good", BaseURL: api.URL + "/v1"}
	if err := ps.Create(ctx, pv); err != nil {
		t.Fatal(err)
	}
	engine := providerEngine(NewProviderHandler(ps), protocol.UserInfo{ID: store.LocalUserID, Role: store.RoleAdmin})

	list := func() []providers.ModelInfo {
		rec := serve(engine, httptest.NewRequest(http.MethodGet, "/providers/"+pv.ID+"/models", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("models = %d %s", rec.Code, rec.Body.String())
		}
		var out []providers.ModelInfo
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	if got := list(); len(got) != 2 || got[0].ID != "gpt-a" {
		t.Fatalf("models = %+v, want gpt-a and gpt-b", got)
	}
	list()
	if calls.Load() != 1 {
		t.Fatalf("the provider was asked %d times within the cache window, want 1", calls.Load())
	}
	// A saved change asks again: the cache is keyed by the row's last change.
	pv.Name = "p2"
	if err := ps.Update(ctx, pv.ID, pv, nil); err != nil {
		t.Fatal(err)
	}
	list()
	if calls.Load() != 2 {
		t.Fatalf("the provider was asked %d times after an edit, want 2", calls.Load())
	}

	// Test: ok with the count; a named model is looked up in the listing.
	rec := serve(engine, httptest.NewRequest(http.MethodPost, "/providers/"+pv.ID+"/test", strings.NewReader(`{"model":"gpt-b"}`)))
	var tr providerTestResp
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &tr) != nil || !tr.OK || tr.ModelCount != 2 || tr.ModelFound == nil || !*tr.ModelFound {
		t.Fatalf("test = %d %s", rec.Code, rec.Body.String())
	}
	rec = serve(engine, httptest.NewRequest(http.MethodPost, "/providers/"+pv.ID+"/test", strings.NewReader(`{"model":"gpt-z"}`)))
	if json.Unmarshal(rec.Body.Bytes(), &tr) != nil || !tr.OK || tr.ModelFound == nil || *tr.ModelFound {
		t.Fatalf("test with an unknown model = %s, want ok with model_found false", rec.Body.String())
	}
}

func TestProviderTestReportsBadKeyAs502(t *testing.T) {
	ctx := context.Background()
	db := testdb.New(t)
	var calls atomic.Int32
	api := fakeModelsAPI(t, "sk-good", &calls)
	defer api.Close()
	ps := store.NewProviderStore(db)
	pv := &store.Provider{OwnerID: store.LocalUserID, Name: "p", APIKey: "sk-wrong-secret", BaseURL: api.URL + "/v1"}
	if err := ps.Create(ctx, pv); err != nil {
		t.Fatal(err)
	}
	engine := providerEngine(NewProviderHandler(ps), protocol.UserInfo{ID: store.LocalUserID, Role: store.RoleAdmin})
	for _, p := range []struct{ method, path string }{
		{http.MethodPost, "/providers/" + pv.ID + "/test"},
		{http.MethodGet, "/providers/" + pv.ID + "/models"},
	} {
		rec := serve(engine, httptest.NewRequest(p.method, p.path, nil))
		if rec.Code != http.StatusBadGateway {
			t.Fatalf("%s %s = %d %s, want 502", p.method, p.path, rec.Code, rec.Body.String())
		}
		// The backend echoed the key and the URL names the host; the answer
		// carries neither — only what the provider answered.
		body := rec.Body.String()
		if strings.Contains(body, "sk-wrong-secret") || strings.Contains(body, api.URL) || strings.Contains(body, "Incorrect API key") {
			t.Fatalf("the error repeats the key, the URL or the body: %s", body)
		}
		if !strings.Contains(body, "401 Unauthorized") {
			t.Fatalf("the error does not say what the provider answered: %s", body)
		}
	}
}

func TestProviderModelsForeignRowIs404(t *testing.T) {
	ctx := context.Background()
	db := testdb.New(t)
	ps := store.NewProviderStore(db)
	owner := store.NewID()
	if _, err := db.NewInsert().Model(&store.User{ID: owner, Email: "o@example.com", Role: store.RoleMember}).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	pv := &store.Provider{OwnerID: owner, Name: "theirs", APIKey: "sk-x", BaseURL: "http://127.0.0.1:9/v1"}
	if err := ps.Create(ctx, pv); err != nil {
		t.Fatal(err)
	}
	stranger := protocol.UserInfo{ID: store.NewID(), Role: store.RoleMember}
	engine := providerEngine(NewProviderHandler(ps), stranger)
	for _, p := range []struct{ method, path string }{
		{http.MethodGet, "/providers/" + pv.ID + "/models"},
		{http.MethodPost, "/providers/" + pv.ID + "/test"},
	} {
		if rec := serve(engine, httptest.NewRequest(p.method, p.path, nil)); rec.Code != http.StatusNotFound {
			t.Fatalf("%s %s by a stranger = %d, want 404", p.method, p.path, rec.Code)
		}
	}
}

func TestProviderModelsChatGPTLoginIs409(t *testing.T) {
	ctx := context.Background()
	db := testdb.New(t)
	ps := store.NewProviderStore(db)
	pv := &store.Provider{OwnerID: store.LocalUserID, Name: "codex", AuthMode: providers.AuthModeChatGPTLogin}
	if err := ps.Create(ctx, pv); err != nil {
		t.Fatal(err)
	}
	engine := providerEngine(NewProviderHandler(ps), protocol.UserInfo{ID: store.LocalUserID, Role: store.RoleAdmin})
	if rec := serve(engine, httptest.NewRequest(http.MethodGet, "/providers/"+pv.ID+"/models", nil)); rec.Code != http.StatusConflict {
		t.Fatalf("models of a ChatGPT-login provider = %d %s, want 409", rec.Code, rec.Body.String())
	}
	// Test reports the login state instead of listing.
	rec := serve(engine, httptest.NewRequest(http.MethodPost, "/providers/"+pv.ID+"/test", nil))
	var tr providerTestResp
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &tr) != nil || tr.OK || tr.Detail != "chatgpt_not_logged_in" {
		t.Fatalf("test of a ChatGPT-login provider = %d %s", rec.Code, rec.Body.String())
	}
}
