package docker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeDaemon is the slice of the docker API the managed-container entry
// points touch: ping, inspect-by-name, stop and remove. It records which
// container identifier each action targeted.
func fakeDaemon(t *testing.T) (host string, acted *[]string) {
	t.Helper()
	var mu sync.Mutex
	var actions []string
	record := func(s string) {
		mu.Lock()
		defer mu.Unlock()
		actions = append(actions, s)
	}
	inspect := func(w http.ResponseWriter, id string, labels map[string]string) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"Id":     id,
			"Config": map[string]any{"Labels": labels},
		})
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimSuffix(r.URL.Path, "/")
		switch {
		case strings.HasSuffix(p, "/_ping"):
			w.Header().Set("API-Version", "1.44")
			w.WriteHeader(http.StatusOK)
		case strings.HasSuffix(p, "/containers/ours/json"):
			inspect(w, "ours-id", map[string]string{fingerprintLabel: "fp"})
		case strings.HasSuffix(p, "/containers/foreign/json"):
			inspect(w, "foreign-id", map[string]string{})
		case strings.HasSuffix(p, "/stop"):
			parts := strings.Split(p, "/")
			record("stop " + parts[len(parts)-2])
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodDelete:
			parts := strings.Split(p, "/")
			record("remove " + parts[len(parts)-1])
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return "tcp://" + srv.Listener.Addr().String(), &actions
}

// Stop and remove refuse a foreign holder of the name, and act on the
// INSPECTED id — never the name, which a remove+recreate race could hand to
// a foreign container between the check and the act.
func TestManagedActionsVerifyOwnershipAndActByID(t *testing.T) {
	host, acted := fakeDaemon(t)
	ctx := context.Background()
	opts := Options{Host: host}

	if err := StopManaged(ctx, opts, "foreign"); err == nil || !strings.Contains(err.Error(), "not created by this package") {
		t.Fatalf("stop of a foreign container = %v, want an ownership refusal", err)
	}
	if err := RemoveManaged(ctx, opts, "foreign"); err == nil || !strings.Contains(err.Error(), "not created by this package") {
		t.Fatalf("remove of a foreign container = %v, want an ownership refusal", err)
	}
	if len(*acted) != 0 {
		t.Fatalf("refused actions still reached the daemon: %v", *acted)
	}

	if err := StopManaged(ctx, opts, "ours"); err != nil {
		t.Fatal(err)
	}
	if err := RemoveManaged(ctx, opts, "ours"); err != nil {
		t.Fatal(err)
	}
	want := []string{"stop ours-id", "remove ours-id"}
	if len(*acted) != 2 || (*acted)[0] != want[0] || (*acted)[1] != want[1] {
		t.Fatalf("actions = %v, want %v (by id, not name)", *acted, want)
	}
}
