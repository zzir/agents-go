package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func googleID(subject, email string) OAuthIdentity {
	return OAuthIdentity{Provider: "google", Subject: subject, Email: email, Name: "N " + subject}
}

// The merge matrix: first login founds an admin account, later emails are
// members, a repeat subject is the same account, and a second provider with
// the same verified email folds into the existing account.
func TestResolveOAuthLoginMergeMatrix(t *testing.T) {
	ctx := context.Background()
	s := NewUserStore(newTestDB(t))

	first, err := s.ResolveOAuthLogin(ctx, googleID("sub-1", "A@example.com"), "")
	if err != nil {
		t.Fatalf("first login: %v", err)
	}
	if first.Role != RoleAdmin {
		t.Fatalf("first account role = %q, want admin", first.Role)
	}
	if first.Email != "a@example.com" {
		t.Fatalf("email not lowercased: %q", first.Email)
	}

	second, err := s.ResolveOAuthLogin(ctx, googleID("sub-2", "b@example.com"), "")
	if err != nil {
		t.Fatalf("second login: %v", err)
	}
	if second.Role != RoleMember {
		t.Fatalf("second account role = %q, want member", second.Role)
	}

	again, err := s.ResolveOAuthLogin(ctx, googleID("sub-1", "a@example.com"), "")
	if err != nil {
		t.Fatalf("repeat login: %v", err)
	}
	if again.ID != first.ID {
		t.Fatal("repeat subject must resolve to the same account")
	}

	// A different provider reporting the same verified email merges. The fake
	// second provider stands in for GitHub, which lands after P2a — the merge
	// path must not wait for it to be exercised.
	merged, err := s.ResolveOAuthLogin(ctx, OAuthIdentity{Provider: "fake", Subject: "x-9", Email: "a@example.com"}, "")
	if err != nil {
		t.Fatalf("merge login: %v", err)
	}
	if merged.ID != first.ID {
		t.Fatal("same verified email must merge into the existing account")
	}
	db := s.db
	n, err := db.NewSelect().Model((*Identity)(nil)).Where("user_id = ?", first.ID).Count(ctx)
	if err != nil || n != 2 {
		t.Fatalf("identities for merged account = %d (%v), want 2", n, err)
	}
}

// The local user must not count toward "first account is admin", and
// --bootstrap-admin promotes on login even when accounts already exist.
func TestResolveOAuthLoginAdminRules(t *testing.T) {
	ctx := context.Background()
	s := NewUserStore(newTestDB(t))

	if _, err := s.EnsureLocalUser(ctx); err != nil {
		t.Fatalf("local user: %v", err)
	}
	u, err := s.ResolveOAuthLogin(ctx, googleID("sub-1", "a@example.com"), "")
	if err != nil || u.Role != RoleAdmin {
		t.Fatalf("first oauth account after local = %v role %q, want admin", err, u.Role)
	}

	member, err := s.ResolveOAuthLogin(ctx, googleID("sub-2", "b@example.com"), "")
	if err != nil || member.Role != RoleMember {
		t.Fatalf("second account = %v role %q, want member", err, member.Role)
	}
	promoted, err := s.ResolveOAuthLogin(ctx, googleID("sub-2", "b@example.com"), "B@example.com")
	if err != nil || promoted.Role != RoleAdmin {
		t.Fatalf("bootstrap-admin login = %v role %q, want admin", err, promoted.Role)
	}

	if _, err := s.ResolveOAuthLogin(ctx, googleID("sub-3", ""), ""); err == nil {
		t.Fatal("a login with no verified email must be refused")
	}
}

// With a bootstrap admin named, nobody becomes admin by being first: the
// first stranger on an allowed domain is a member, the named one is admin
// whenever they arrive.
func TestBootstrapAdminSuppressesFirstAccountRule(t *testing.T) {
	ctx := context.Background()
	s := NewUserStore(newTestDB(t))

	first, err := s.ResolveOAuthLogin(ctx, googleID("sub-1", "eve@example.com"), "alice@example.com")
	if err != nil || first.Role != RoleMember {
		t.Fatalf("first login while a bootstrap admin is named = %v role %q, want member", err, first.Role)
	}
	alice, err := s.ResolveOAuthLogin(ctx, googleID("sub-2", "Alice@example.com"), "alice@example.com")
	if err != nil || alice.Role != RoleAdmin {
		t.Fatalf("the bootstrap admin's login = %v role %q, want admin", err, alice.Role)
	}
	if n, err := s.CountReal(ctx); err != nil || n != 2 {
		t.Fatalf("CountReal = %d, %v; want 2", n, err)
	}
}

func TestAuthTokensLifecycle(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	users := NewUserStore(db)
	tokens := NewAuthTokenStore(db)
	u, err := users.EnsureLocalUser(ctx)
	if err != nil {
		t.Fatalf("user: %v", err)
	}

	secret, minted, err := tokens.Mint(ctx, u.ID, TokenKindSession, "", time.Time{})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if minted.ExpiresAt.IsZero() {
		t.Fatal("a session token must carry the sliding expiry")
	}
	got, tok, err := tokens.Authenticate(ctx, secret)
	if err != nil || got.ID != u.ID {
		t.Fatalf("authenticate: %v (user %+v)", err, got)
	}
	slid := tok.ExpiresAt

	// Immediately again: inside the slide throttle, the row must not rewrite.
	_, tok2, err := tokens.Authenticate(ctx, secret)
	if err != nil {
		t.Fatalf("authenticate again: %v", err)
	}
	if !tok2.ExpiresAt.Equal(slid) {
		t.Fatal("expiry slid inside the throttle window")
	}

	if _, _, err := tokens.Authenticate(ctx, "ags_s_wrong"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("wrong token = %v, want ErrNotFound", err)
	}

	// Logout revokes the presented token.
	if err := tokens.RevokeByPlaintext(ctx, secret); err != nil {
		t.Fatalf("logout: %v", err)
	}
	if _, _, err := tokens.Authenticate(ctx, secret); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after logout = %v, want ErrNotFound", err)
	}
}

func TestPATExpiryAndRevoke(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	tokens := NewAuthTokenStore(db)
	u, err := NewUserStore(db).EnsureLocalUser(ctx)
	if err != nil {
		t.Fatalf("user: %v", err)
	}

	forever, _, err := tokens.Mint(ctx, u.ID, TokenKindPAT, "ci", time.Time{})
	if err != nil {
		t.Fatalf("mint pat: %v", err)
	}
	if _, _, err := tokens.Authenticate(ctx, forever); err != nil {
		t.Fatalf("a PAT without expiry must authenticate: %v", err)
	}

	expired, _, err := tokens.Mint(ctx, u.ID, TokenKindPAT, "old", time.Now().UTC().Add(-time.Minute))
	if err != nil {
		t.Fatalf("mint expired pat: %v", err)
	}
	if _, _, err := tokens.Authenticate(ctx, expired); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired PAT = %v, want ErrNotFound", err)
	}

	// Revoke is self-service only: someone else's user id deletes nothing.
	list, err := tokens.ListByUser(ctx, u.ID, TokenKindPAT)
	if err != nil || len(list) != 1 { // the expired one was deleted in passing
		t.Fatalf("list = %d tokens (%v), want 1", len(list), err)
	}
	if err := tokens.Revoke(ctx, list[0].ID, NewID()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-user revoke = %v, want ErrNotFound", err)
	}
	if err := tokens.Revoke(ctx, list[0].ID, u.ID); err != nil {
		t.Fatalf("own revoke: %v", err)
	}
	if _, _, err := tokens.Authenticate(ctx, forever); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after revoke = %v, want ErrNotFound", err)
	}
}
