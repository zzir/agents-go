package sessiontest

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/zzir/agents-go/agents/session"
)

// RepoUnderTest is one backend's answer to "give me an empty repo".
type RepoUnderTest struct {
	// Repo is the implementation being checked.
	Repo session.Repo

	// IDs maps the suite's literal session names ("x", "shared") to ids the
	// backend accepts, the same id per name for a whole check; nil uses the
	// names (spec §2.5e).
	IDs func(name string) string

	// Direct opens a session through the backend's NON-repo constructor
	// (sessions.New), where the id names the storage; nil skips those checks —
	// spec §2.5e2.
	Direct func(id string) (*session.Session, error)
}

// RepoConformance holds a Repo to the parts of spec §2.5e2 a repo owns: that
// it addresses a session by session.Ref rather than its id, and what its
// listing says about the sessions it holds.
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
	{"ADeletedHandleRefusesEveryWrite", checkDeletedHandleRefusesEveryWrite},
	{"DeleteLeavesTheDirectScopeAlone", checkDeleteVsDirect},
	{"DirectAndRepoDoNotShareHistory", checkDirectIsolation},
	{"ListIsNewestFirst", checkListNewestFirst},
	{"ListHonoursLimit", checkListLimit},
	{"AServedSessionNeverCreatesItsParent", checkServedSessionParent},
}

// id resolves a suite-literal session name through IDs.
func (r RepoUnderTest) id(name string) string {
	if r.IDs == nil {
		return name
	}
	return r.IDs(name)
}

func repoWrite(t *testing.T, sess *session.Session, text string) {
	t.Helper()
	item, err := session.UnmarshalInputItem([]byte(`{"role":"user","content":"` + text + `"}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.AppendItems(context.Background(), []session.InputItem{item}, session.Source{}); err != nil {
		t.Fatalf("append %q: %v", text, err)
	}
}

func repoTexts(t *testing.T, sess *session.Session) []string {
	t.Helper()
	entries, err := sess.Entries(context.Background(), session.Cursor{})
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
	sess, err := r.Repo.Create(ctx, session.CreateOptions{ID: r.id("x"), Title: "A chat"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	repoWrite(t, sess, "hello")

	again, err := r.Repo.Open(ctx, r.id("x"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if got := repoTexts(t, again); len(got) != 1 {
		t.Fatalf("reopening read %v, want the one entry written", got)
	}
}

func checkOpenUnknown(t *testing.T, r RepoUnderTest) {
	t.Helper()
	_, err := r.Repo.Open(context.Background(), r.id("never-created"))
	if err == nil {
		t.Fatal("opening an unknown session succeeded")
	}
}

// A hidden session names the session it serves; a repo may ignore the name
// or refuse an unknown one, but it never conjures the parent (spec §2.5e).
func checkServedSessionParent(t *testing.T, r RepoUnderTest) {
	t.Helper()
	ctx := context.Background()
	if _, err := r.Repo.Create(ctx, session.CreateOptions{ID: r.id("parent"), Title: "parent"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Repo.Create(ctx, session.CreateOptions{ID: r.id("child"), Hidden: true, ParentID: r.id("parent")}); err != nil {
		t.Fatalf("creating a served session under an existing parent: %v", err)
	}
	_, err := r.Repo.Create(ctx, session.CreateOptions{ID: r.id("orphan"), Hidden: true, ParentID: r.id("never-created")})
	if err != nil && !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("an unknown parent must be ignored or refused with ErrNotFound, got: %v", err)
	}
	if _, err := r.Repo.Open(ctx, r.id("never-created")); err == nil {
		t.Fatal("creating a served session brought its unknown parent into existence")
	}
}

func checkDeleteUnknown(t *testing.T, r RepoUnderTest) {
	t.Helper()
	if err := r.Repo.Delete(context.Background(), r.id("never-created")); err != nil {
		t.Fatalf("deleting an unknown session: %v", err)
	}
}

// A handle to a deleted session must not follow its id onto the next one. It is
// deliberately NOT used before the delete: binding is at build time — spec §2.5e2.
func checkRecreatedID(t *testing.T, r RepoUnderTest) {
	t.Helper()
	ctx := context.Background()
	if _, err := r.Repo.Create(ctx, session.CreateOptions{ID: r.id("x")}); err != nil {
		t.Fatalf("create: %v", err)
	}
	stale, err := r.Repo.Open(ctx, r.id("x"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	if err := r.Repo.Delete(ctx, r.id("x")); err != nil {
		t.Fatalf("delete: %v", err)
	}
	fresh, err := r.Repo.Create(ctx, session.CreateOptions{ID: r.id("x")})
	if err != nil {
		t.Fatalf("recreate: %v", err)
	}
	repoWrite(t, fresh, "secret")

	if got := repoTexts(t, stale); len(got) != 0 {
		t.Fatalf("a handle to the deleted session reads the new one's history: %v", got)
	}
	// A write through the stale handle REFUSES; it never lands quietly
	// elsewhere — spec §2.5e2.
	item, err := session.UnmarshalInputItem([]byte(`{"role":"user","content":"from the dead"}`))
	if err != nil {
		t.Fatal(err)
	}
	werr := stale.AppendItems(ctx, []session.InputItem{item}, session.Source{})
	if werr == nil || !errors.Is(werr, session.ErrNotFound) {
		t.Fatalf("a write through a handle to a deleted session must refuse with ErrSessionNotFound, got: %v", werr)
	}
	if got := repoTexts(t, fresh); len(got) != 1 {
		t.Fatalf("the new session was disturbed by the stale handle: %v", got)
	}
}

// EVERY write refuses, not just the one a test reached for: a capability the
// inner store gains must not bypass the wrapper's existence proof (spec §2.5e2).
func checkDeletedHandleRefusesEveryWrite(t *testing.T, r RepoUnderTest) {
	t.Helper()
	ctx := context.Background()
	sess, err := r.Repo.Create(ctx, session.CreateOptions{ID: r.id("x")})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	repoWrite(t, sess, "written while it existed")
	st := sess.Storage()
	if err := r.Repo.Delete(ctx, r.id("x")); err != nil {
		t.Fatalf("delete: %v", err)
	}

	refuses := func(what string, err error) {
		t.Helper()
		if !errors.Is(err, session.ErrNotFound) {
			t.Errorf("%s through a handle to a deleted session must refuse with ErrNotFound, got: %v", what, err)
		}
	}
	refuses("Append", st.Append(ctx, storageItem(t, "from the dead")))
	refuses("Clear", st.Clear(ctx))
	if replacer, ok := st.(session.AtomicReplacer); ok {
		refuses("ReplaceEntries", replacer.ReplaceEntries(ctx, storageItem(t, "from the dead")))
	}
	listed, err := r.Repo.List(ctx, session.ListOptions{IncludeHidden: true})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(listed) != 0 {
		t.Fatalf("a refused write left %d session(s) behind: %+v", len(listed), listed)
	}
}

// The same rule for what a handle SAYS about itself: a stale one must not
// answer with the replacement's title and timestamps.
func checkStaleHandleMetadata(t *testing.T, r RepoUnderTest) {
	t.Helper()
	ctx := context.Background()
	if _, err := r.Repo.Create(ctx, session.CreateOptions{ID: r.id("x"), Title: "first"}); err != nil {
		t.Fatal(err)
	}
	stale, err := r.Repo.Open(ctx, r.id("x"))
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Repo.Delete(ctx, r.id("x")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Repo.Create(ctx, session.CreateOptions{ID: r.id("x"), Title: "second"}); err != nil {
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
	direct, err := r.Direct(r.id("orphan"))
	if err != nil {
		t.Fatal(err)
	}
	repoWrite(t, direct, "written directly")
	if err := r.Repo.Delete(ctx, r.id("orphan")); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if got := repoTexts(t, direct); len(got) != 1 {
		t.Fatalf("deleting an id the repo does not have emptied the direct session: %v", got)
	}

	// And with a repo session of the same id present, deleting it takes only
	// its own history.
	shared, err := r.Direct(r.id("shared"))
	if err != nil {
		t.Fatal(err)
	}
	repoWrite(t, shared, "written directly")
	if _, err := r.Repo.Create(ctx, session.CreateOptions{ID: r.id("shared")}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := r.Repo.Delete(ctx, r.id("shared")); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if got := repoTexts(t, shared); len(got) != 1 {
		t.Fatalf("the repo's delete took the direct session's history: %v", got)
	}
}

// repoSessionsNewestFirst creates one session per id, then writes to them in
// REVERSE: List owes the ids as given, and creation order is the opposite, so a
// CreatedAt sort fails. The pause keeps the coarsest backend clock from tying
// the stamps.
func repoSessionsNewestFirst(t *testing.T, r RepoUnderTest, ids ...string) []string {
	t.Helper()
	ctx := context.Background()
	handles := make([]*session.Session, len(ids))
	mapped := make([]string, len(ids))
	for i, id := range ids {
		mapped[i] = r.id(id)
		sess, err := r.Repo.Create(ctx, session.CreateOptions{ID: mapped[i]})
		if err != nil {
			t.Fatalf("create %q: %v", id, err)
		}
		handles[i] = sess
	}
	for i, handle := range slices.Backward(handles) {
		repoWrite(t, handle, "hello")
		if i > 0 {
			time.Sleep(10 * time.Millisecond)
		}
	}
	return mapped
}

func metadataIDs(md []session.Metadata) []string {
	out := make([]string, 0, len(md))
	for _, m := range md {
		out = append(out, m.ID)
	}
	return out
}

// A listing is ordered by last change, newest first: the order Limit truncates
// (spec §2.5e2).
func checkListNewestFirst(t *testing.T, r RepoUnderTest) {
	t.Helper()
	want := repoSessionsNewestFirst(t, r, "newest", "middle", "oldest")

	md, err := r.Repo.List(context.Background(), session.ListOptions{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if got := metadataIDs(md); !slices.Equal(got, want) {
		t.Fatalf("List = %v, want %v (newest first)", got, want)
	}
	// The stamps agree with the order: sorting by one column and reporting
	// another is a coincidence.
	for i := 1; i < len(md); i++ {
		if md[i].UpdatedAt.After(md[i-1].UpdatedAt) {
			t.Fatalf("List is not ordered by UpdatedAt: %s at %v precedes %s at %v",
				md[i-1].ID, md[i-1].UpdatedAt, md[i].ID, md[i].UpdatedAt)
		}
	}
}

// Limit caps the listing from the newest end, after sorting; anything not
// positive is no limit (spec §2.5e2).
func checkListLimit(t *testing.T, r RepoUnderTest) {
	t.Helper()
	ctx := context.Background()
	all := repoSessionsNewestFirst(t, r, "a", "b", "c")

	for _, tc := range []struct {
		name  string
		limit int
		want  []string
	}{
		{"a short page", 2, all[:2]},
		{"the newest alone", 1, all[:1]},
		{"zero is no limit", 0, all},
		{"negative is no limit", -1, all},
		{"more than there are", len(all) + 5, all},
	} {
		t.Run(tc.name, func(t *testing.T) {
			md, err := r.Repo.List(ctx, session.ListOptions{Limit: tc.limit})
			if err != nil {
				t.Fatalf("list: %v", err)
			}
			if got := metadataIDs(md); !slices.Equal(got, tc.want) {
				t.Errorf("List(Limit=%d) = %v, want %v", tc.limit, got, tc.want)
			}
		})
	}
}

func checkDirectIsolation(t *testing.T, r RepoUnderTest) {
	t.Helper()
	if r.Direct == nil {
		t.Skip("no non-repo constructor to isolate from")
	}
	ctx := context.Background()

	sess, err := r.Repo.Create(ctx, session.CreateOptions{ID: r.id("x")})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	repoWrite(t, sess, "through the repo")

	direct, err := r.Direct(r.id("x"))
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
