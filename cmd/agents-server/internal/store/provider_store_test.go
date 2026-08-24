package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Two edits of one provider: the first holds the store's transaction open in
// prepare while the second starts. The second's prepare must see the first's
// key — SQLite's one connection and PostgreSQL's FOR UPDATE both make it wait
// — or a masked-key round-trip would restore a key the other edit replaced.
func TestProviderUpdateSerializesConcurrentEdits(t *testing.T) {
	ctx := context.Background()
	withTestBox(t)
	providers := NewProviderStore(newTestDB(t))
	pv := &Provider{Name: "p", Type: "openai", APIKey: "sk-old"}
	if err := providers.Create(ctx, pv); err != nil {
		t.Fatal(err)
	}

	entered, release := make(chan struct{}), make(chan struct{})
	first := make(chan error, 1)
	go func() {
		rotated := &Provider{Name: "p", Type: "openai", APIKey: "sk-new"}
		first <- providers.Update(ctx, pv.ID, rotated, func(*Provider) error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered

	second := make(chan error, 1)
	var seen string
	go func() {
		masked := &Provider{Name: "p", Type: "openai"}
		second <- providers.Update(ctx, pv.ID, masked, func(prev *Provider) error {
			seen = prev.APIKey
			masked.APIKey = prev.APIKey
			return nil
		})
	}()
	// Long enough for an unlocked read to have happened; a false pass needs
	// the second goroutine to be slower than this to reach its SELECT.
	time.Sleep(50 * time.Millisecond)
	close(release)

	if err := <-first; err != nil {
		t.Fatalf("first update: %v", err)
	}
	if err := <-second; err != nil {
		t.Fatalf("second update: %v", err)
	}
	if seen != "sk-new" {
		t.Fatalf("second edit read api_key %q before the first committed; want sk-new", seen)
	}
	if got, _ := providers.Get(ctx, pv.ID); got.APIKey != "sk-new" {
		t.Fatalf("stored api_key = %q, want sk-new", got.APIKey)
	}
}

func TestProviderUpdatePrepareErrorAbortsAndNotFound(t *testing.T) {
	ctx := context.Background()
	withTestBox(t)
	providers := NewProviderStore(newTestDB(t))
	pv := &Provider{Name: "p", Type: "openai", APIKey: "sk-1"}
	if err := providers.Create(ctx, pv); err != nil {
		t.Fatal(err)
	}
	refused := errors.New("refused")
	err := providers.Update(ctx, pv.ID, &Provider{Name: "renamed", Type: "openai"}, func(prev *Provider) error {
		if prev.APIKey != "sk-1" {
			t.Errorf("prepare saw api_key %q, want the opened plaintext", prev.APIKey)
		}
		return refused
	})
	if !errors.Is(err, refused) {
		t.Fatalf("Update = %v, want the prepare error", err)
	}
	if got, _ := providers.Get(ctx, pv.ID); got.Name != "p" {
		t.Fatalf("a refused update wrote: name = %q", got.Name)
	}
	err = providers.Update(ctx, NewID(), &Provider{Name: "x", Type: "openai"}, func(*Provider) error {
		t.Error("prepare ran for a missing row")
		return nil
	})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Update of a missing row = %v, want ErrNotFound", err)
	}
}

// The chatgpt_token column follows the auth mode: kept across an edit that
// stays chatgpt_login, cleared by one that leaves it.
func TestProviderUpdateChatGPTTokenFollowsAuthMode(t *testing.T) {
	ctx := context.Background()
	withTestBox(t)
	providers := NewProviderStore(newTestDB(t))
	pv := &Provider{Name: "p", Type: "openai", AuthMode: AuthModeChatGPTLogin}
	if err := providers.Create(ctx, pv); err != nil {
		t.Fatal(err)
	}
	if err := providers.SaveChatGPTToken(ctx, pv.ID, `{"access":"tok"}`); err != nil {
		t.Fatal(err)
	}
	if err := providers.Update(ctx, pv.ID, &Provider{Name: "renamed", Type: "openai", AuthMode: AuthModeChatGPTLogin}, nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := providers.Get(ctx, pv.ID); got.ChatGPTToken != `{"access":"tok"}` {
		t.Fatalf("token after a chatgpt_login edit = %q, want kept", got.ChatGPTToken)
	}
	if err := providers.Update(ctx, pv.ID, &Provider{Name: "renamed", Type: "openai", APIKey: "sk-1"}, nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := providers.Get(ctx, pv.ID); got.ChatGPTToken != "" {
		t.Fatalf("token after leaving chatgpt_login = %q, want cleared", got.ChatGPTToken)
	}
}
