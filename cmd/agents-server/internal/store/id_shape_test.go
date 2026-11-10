package store

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// A lookup by something that is not an id is a miss, not a database error:
// PostgreSQL refuses a non-UUID for a uuid column where SQLite finds nothing.
func TestLookupByMalformedIDIsNotFound(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	for _, id := range []string{"nope", "", strings.ToUpper(NewID()), "{" + NewID() + "}"} {
		if _, err := NewAgentConfigStore(db).Get(ctx, id); !errors.Is(err, ErrNotFound) {
			t.Errorf("agent %q: %v, want ErrNotFound", id, err)
		}
		if _, err := NewSessionStore(db).Get(ctx, id); !errors.Is(err, ErrNotFound) {
			t.Errorf("session %q: %v, want ErrNotFound", id, err)
		}
		if _, err := NewUserStore(db).ByID(ctx, id); !errors.Is(err, ErrNotFound) {
			t.Errorf("user %q: %v, want ErrNotFound", id, err)
		}
	}
	if !IsID(NewID()) || !IsID(NewTimeID()) || !IsID(LocalUserID) {
		t.Error("IsID refuses an id the server mints")
	}
}
