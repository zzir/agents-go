package agentstest

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/agents/session"
)

// RepoUnderTest is one backend's answer to "give me an empty repo".
type RepoUnderTest struct {
	// Repo is the implementation being checked.
	Repo session.Repo

	// Direct opens a session by id through the backend's NON-repo constructor
	// — filesession.New, sessions.New — where the id names the storage
	// outright. A backend without one leaves this nil and those checks skip.
	//
	// It is the scope a repo must never reach, in either direction.
	Direct func(id string) (*session.Session, error)
}

// RepoConformance holds a SessionRepo to the parts of the entry lifecycle
// contract in docs/spec.md §2.5e2 that a repo owns: how it addresses a session,
// and what its listing says about the sessions it holds.
//
// Most of it is checking that a backend addresses a session by session.Ref and
// not by its id — every one of those failed at least once in a backend that
// carried the generation as a field some code path forgot. The listing checks
// are here for the same reason: one backend did not sort its listing at all and
// another ignored the limit, each of them fine until a caller switched.
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
}

func repoWrite(t *testing.T, sess *session.Session, text string) {
	t.Helper()
	item, err := session.UnmarshalInputItem([]byte(`{"role":"user","content":"` + text + `"}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.AppendItems(context.Background(), []agents.InputItem{item}, agents.Source{}); err != nil {
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
	sess, err := r.Repo.Create(ctx, session.CreateOptions{ID: "x", Title: "A chat"})
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
	if _, err := r.Repo.Create(ctx, session.CreateOptions{ID: "x"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	stale, err := r.Repo.Open(ctx, "x")
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	if err := r.Repo.Delete(ctx, "x"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	fresh, err := r.Repo.Create(ctx, session.CreateOptions{ID: "x"})
	if err != nil {
		t.Fatalf("recreate: %v", err)
	}
	repoWrite(t, fresh, "secret")

	if got := repoTexts(t, stale); len(got) != 0 {
		t.Fatalf("a handle to the deleted session reads the new one's history: %v", got)
	}
	// A write through the stale handle REFUSES — its destination is gone. A
	// quietly "isolated" write would mint entries nothing references:
	// invisible to every listing, unreachable by Delete, orphaned storage by
	// construction (spec §2.5e2: writing and proving the destination still
	// exists are one step).
	item, err := session.UnmarshalInputItem([]byte(`{"role":"user","content":"from the dead"}`))
	if err != nil {
		t.Fatal(err)
	}
	werr := stale.AppendItems(ctx, []agents.InputItem{item}, agents.Source{})
	if werr == nil || !errors.Is(werr, session.ErrNotFound) {
		t.Fatalf("a write through a handle to a deleted session must refuse with ErrSessionNotFound, got: %v", werr)
	}
	if got := repoTexts(t, fresh); len(got) != 1 {
		t.Fatalf("the new session was disturbed by the stale handle: %v", got)
	}
}

// EVERY write refuses, not just the one a test reached for. A repo's storage
// is usually a wrapper around a plain one, and the wrapper is what proves the
// session still exists — so a capability the inner store gains arrives here on
// its own, past that proof, and writes storage nothing references: invisible to
// a listing, unreachable by Delete (spec §2.5e2).
func checkDeletedHandleRefusesEveryWrite(t *testing.T, r RepoUnderTest) {
	t.Helper()
	ctx := context.Background()
	sess, err := r.Repo.Create(ctx, session.CreateOptions{ID: "x"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	repoWrite(t, sess, "written while it existed")
	st := sess.Storage()
	if err := r.Repo.Delete(ctx, "x"); err != nil {
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
	if g, ok := st.(session.GuardedReplacer); ok {
		_, err := g.ReplaceEntriesIf(ctx, 0, storageItem(t, "from the dead"))
		refuses("ReplaceEntriesIf", err)
	}
	// A pop may instead report that it found nothing: the entries went with the
	// session, so there is no undo to refuse and nothing was written either
	// way. Handing one BACK is the failure — that is the dead session's history
	// being read and mutated through a handle that should not reach it.
	popped := func(what string, e *session.Entry, err error) {
		t.Helper()
		if err == nil && e != nil {
			t.Errorf("%s through a handle to a deleted session removed %+v", what, e)
			return
		}
		if err != nil && !errors.Is(err, session.ErrNotFound) {
			t.Errorf("%s through a handle to a deleted session failed with %v, want ErrNotFound or nothing to pop", what, err)
		}
	}
	if popper, ok := st.(session.EntryPopper); ok {
		e, err := popper.PopEntry(ctx)
		popped("PopEntry", e, err)
	}
	if popper, ok := st.(session.ItemPopper); ok {
		e, err := popper.PopItem(ctx)
		popped("PopItem", e, err)
	}

	listed, err := r.Repo.List(ctx, session.ListOptions{IncludeHidden: true})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(listed) != 0 {
		t.Fatalf("a refused write left %d session(s) behind: %+v", len(listed), listed)
	}
}

// The same rule for what a handle SAYS about itself, not just what it holds. A
// stale one answering with the replacement's title and timestamps reports one
// session's size under another's name.
func checkStaleHandleMetadata(t *testing.T, r RepoUnderTest) {
	t.Helper()
	ctx := context.Background()
	if _, err := r.Repo.Create(ctx, session.CreateOptions{ID: "x", Title: "first"}); err != nil {
		t.Fatal(err)
	}
	stale, err := r.Repo.Open(ctx, "x")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Repo.Delete(ctx, "x"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Repo.Create(ctx, session.CreateOptions{ID: "x", Title: "second"}); err != nil {
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
	if _, err := r.Repo.Create(ctx, session.CreateOptions{ID: "shared"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := r.Repo.Delete(ctx, "shared"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if got := repoTexts(t, shared); len(got) != 1 {
		t.Fatalf("the repo's delete took the direct session's history: %v", got)
	}
}

// repoSessionsNewestFirst creates one session per id and then writes to them in
// REVERSE, so the order List owes back — last changed first — is the ids as
// given, and the order they were created in is its opposite. Creating and
// writing in one pass would make the two agree, and then a backend sorting by
// CreatedAt would satisfy every check below without ever reading UpdatedAt.
//
// The pause is what makes the stamps distinct. The backends time a write by
// three different clocks — a wall clock, a file's mtime, a database timestamp —
// and the coarsest of them decides whether two writes microseconds apart can be
// told apart at all. Ties are unordered by contract, so without it these checks
// would pass on any order a backend felt like.
func repoSessionsNewestFirst(t *testing.T, r RepoUnderTest, ids ...string) []string {
	t.Helper()
	ctx := context.Background()
	handles := make([]*session.Session, len(ids))
	for i, id := range ids {
		sess, err := r.Repo.Create(ctx, session.CreateOptions{ID: id})
		if err != nil {
			t.Fatalf("create %q: %v", id, err)
		}
		handles[i] = sess
	}
	for i := len(handles) - 1; i >= 0; i-- {
		repoWrite(t, handles[i], "hello")
		if i > 0 {
			time.Sleep(10 * time.Millisecond)
		}
	}
	return slices.Clone(ids)
}

func metadataIDs(md []session.Metadata) []string {
	out := make([]string, 0, len(md))
	for _, m := range md {
		out = append(out, m.ID)
	}
	return out
}

// A listing is ordered by last change, newest first — the order a sidebar shows
// and the order Limit truncates, so a backend sorting by creation (or not at
// all) hands the caller a different conversation than every other backend does.
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
	// And the stamps agree with the order: a backend that sorts by one column
	// and reports another is right here only by coincidence.
	for i := 1; i < len(md); i++ {
		if md[i].UpdatedAt.After(md[i-1].UpdatedAt) {
			t.Fatalf("List is not ordered by UpdatedAt: %s at %v precedes %s at %v",
				md[i-1].ID, md[i-1].UpdatedAt, md[i].ID, md[i].UpdatedAt)
		}
	}
}

// Limit caps the listing, and it caps it from the newest end — a backend that
// applies it before sorting, or ignores it, returns the wrong page rather than
// a short one. Anything not positive means no limit.
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
			md, err := r.Repo.List(ctx, session.ListOptions{Cursor: session.Cursor{Limit: tc.limit}})
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

	sess, err := r.Repo.Create(ctx, session.CreateOptions{ID: "x"})
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
