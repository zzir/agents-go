package handler

import (
	"context"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/uptrace/bun"

	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// newTestDB opens an isolated in-memory SQLite database with the full schema
// created, mirroring the store package's test helper. Each call gets its own
// database (unique shared-cache name), closed via t.Cleanup.
func newTestDB(t *testing.T) *bun.DB {
	t.Helper()
	db, err := store.NewSQLiteDB("file:" + store.NewID() + "?mode=memory&cache=shared")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.CreateSchema(context.Background(), db); err != nil {
		t.Fatalf("schema: %v", err)
	}
	return db
}

// doJSON performs an in-process HTTP request with an optional JSON body
// against engine and returns the recorded response.
func doJSON(t *testing.T, engine *gin.Engine, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rdr)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)
	return w
}
