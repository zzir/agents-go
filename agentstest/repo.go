package agentstest

import (
	"context"
	"testing"

	"github.com/zzir/agents-go/agents"
)

// RepoUnderTest is one backend's answer to "give me an empty repo".
type RepoUnderTest struct {
	// Repo is the implementation being checked.
	Repo agents.SessionRepo

	// Direct opens a session by id through the backend's NON-repo constructor
	// — memory.NewFileSession, sessions.New — where the id names the storage
	// outright. A backend without one leaves this nil and those checks skip.
	//
	// It is the scope a repo must never reach, in either direction.
	Direct func(id string) (*agents.Session, error)
}

// RepoConformance holds a SessionRepo to the identity half of the entry
// lifecycle contract in docs/spec.md §2.5e2.
//
// What it is really checking is that a backend addresses a session by
// agents.SessionRef and not by its id — every one of these failed at least once
// in a backend that carried the generation as a field some code path forgot.
func RepoConformance(t *testing.T, newRepo func(t *testing.T) RepoUnderTest) {
	t.Helper()
	for _, c := range repoChecks {
		t.Run(c.name, func(t *testing.T) { c.run(t, newRepo(t)) })
	}
}

var repoChecks = []struct {
	name string
	run  func(t *testing.T, r RepoUnderTest)
}{
	{"CreateThenOpen", checkCreateThenOpen},
	{"OpenUnknownIsNotFound", checkOpenUnknown},
	{"DeleteUnknownSucceeds", checkDeleteUnknown},
	{"ARecreatedIDIsANewSession", checkRecreatedID},
	{"AStaleHandleSeesOnlyItsOwn", checkStaleHandleMetadata},
	{"DeleteLeavesTheDirectScopeAlone", checkDeleteVsDirect},
	{"DirectAndRepoDoNotShareHistory", checkDirectIsolation},
}

func repoWrite(t *testing.T, sess *agents.Session, text string) {
	t.Helper()
	item, err := agents.UnmarshalInputItem([]byte(`{"role":"user","content":"` + text + `"}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.AppendItems(context.Background(), []agents.TResponseInputItem{item}, agents.Source{}); err != nil {
		t.Fatalf("append %q: %v", text, err)
	}
}

func repoTexts(t *testing.T, sess *agents.Session) []string {
	t.Helper()
	entries, err := sess.Entries(context.Background(), agents.Cursor{})
	if err != nil {
		t.Fatalf("entries: %v", err)
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, string(e.Item))
	}
	return out
}

func checkCreateThenOpen(t *testing.T, r RepoUnderTest) {
	t.Helper()
	ctx := context.Background()
	sess, err := r.Repo.Create(ctx, agents.CreateOptions{ID: "x", Title: "A chat"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	repoWrite(t, sess, "hello")

	again, err := r.Repo.Open(ctx, "x")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if got := repoTexts(t, again); len(got) != 1 {
		t.Fatalf("reopening read %v, want the one entry written", got)
	}
}

func checkOpenUnknown(t *testing.T, r RepoUnderTest) {
	t.Helper()
	_, err := r.Repo.Open(context.Background(), "never-created")
	if err == nil {
		t.Fatal("opening an unknown session succeeded")
	}
}

func checkDeleteUnknown(t *testing.T, r RepoUnderTest) {
	t.Helper()
	if err := r.Repo.Delete(context.Background(), "never-created"); err != nil {
		t.Fatalf("deleting an unknown session: %v", err)
	}
}

// A handle to a deleted session must not follow its id onto the next one.
//
// The handle is deliberately NOT used before the delete: a backend that binds
// its scope on first use rather than when the handle is built passes only when
// a test happens to touch it early, which is how that stayed broken through two
// rounds of review.
func checkRecreatedID(t *testing.T, r RepoUnderTest) {
	t.Helper()
	ctx := context.Background()
	if _, err := r.Repo.Create(ctx, agents.CreateOptions{ID: "x"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	stale, err := r.Repo.Open(ctx, "x")
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	if err := r.Repo.Delete(ctx, "x"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	fresh, err := r.Repo.Create(ctx, agents.CreateOptions{ID: "x"})
	if err != nil {
		t.Fatalf("recreate: %v", err)
	}
	repoWrite(t, fresh, "secret")

	if got := repoTexts(t, stale); len(got) != 0 {
		t.Fatalf("a handle to the deleted session reads the new one's history: %v", got)
	}
	repoWrite(t, stale, "from the dead")
	if got := repoTexts(t, fresh); len(got) != 1 {
		t.Fatalf("the new session absorbed the stale handle's writes: %v", got)
	}
}

// The same rule for what a handle SAYS about itself, not just what it holds. A
// stale one answering with the replacement's title and timestamps reports one
// session's size under another's name.
func checkStaleHandleMetadata(t *testing.T, r RepoUnderTest) {
	t.Helper()
	ctx := context.Background()
	if _, err := r.Repo.Create(ctx, agents.CreateOptions{ID: "x", Title: "first"}); err != nil {
		t.Fatal(err)
	}
	stale, err := r.Repo.Open(ctx, "x")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Repo.Delete(ctx, "x"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Repo.Create(ctx, agents.CreateOptions{ID: "x", Title: "second"}); err != nil {
		t.Fatal(err)
	}

	md, err := stale.Metadata(ctx)
	if err != nil {
		// Refusing is a fine answer: the session it addresses is gone.
		return
	}
	if md.Title == "second" {
		t.Fatalf("a stale handle reports the replacement's metadata: %+v", md)
	}
}

func checkDeleteVsDirect(t *testing.T, r RepoUnderTest) {
	t.Helper()
	if r.Direct == nil {
		t.Skip("no non-repo constructor to isolate from")
	}
	ctx := context.Background()

	// An id the repo has never heard of.
	direct, err := r.Direct("orphan")
	if err != nil {
		t.Fatal(err)
	}
	repoWrite(t, direct, "written directly")
	if err := r.Repo.Delete(ctx, "orphan"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if got := repoTexts(t, direct); len(got) != 1 {
		t.Fatalf("deleting an id the repo does not have emptied the direct session: %v", got)
	}

	// And with a repo session of the same id present, deleting it takes only
	// its own history.
	shared, err := r.Direct("shared")
	if err != nil {
		t.Fatal(err)
	}
	repoWrite(t, shared, "written directly")
	if _, err := r.Repo.Create(ctx, agents.CreateOptions{ID: "shared"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := r.Repo.Delete(ctx, "shared"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if got := repoTexts(t, shared); len(got) != 1 {
		t.Fatalf("the repo's delete took the direct session's history: %v", got)
	}
}

func checkDirectIsolation(t *testing.T, r RepoUnderTest) {
	t.Helper()
	if r.Direct == nil {
		t.Skip("no non-repo constructor to isolate from")
	}
	ctx := context.Background()

	sess, err := r.Repo.Create(ctx, agents.CreateOptions{ID: "x"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	repoWrite(t, sess, "through the repo")

	direct, err := r.Direct("x")
	if err != nil {
		t.Fatal(err)
	}
	if got := repoTexts(t, direct); len(got) != 0 {
		t.Fatalf("the direct scope reads the repo session's history: %v", got)
	}
	repoWrite(t, direct, "directly")
	if got := repoTexts(t, sess); len(got) != 1 {
		t.Fatalf("a direct write reached the repo session: %v", got)
	}
}
