package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect"
)

// User roles (README "Ownership and roles").
const (
	RoleAdmin  = "admin"
	RoleMember = "member"
)

// AuthToken kinds: a browser session (sliding expiry) or a personal access
// token (optional fixed expiry).
const (
	TokenKindSession = "session"
	TokenKindPAT     = "pat"
)

// LocalUserID is the implicit account behind --auth token mode: one machine,
// one person, full access. Ownership columns reference it so both auth modes
// share one data model. A fixed UUID, as every id column is uuid-typed.
const LocalUserID = "00000000-0000-0000-0000-000000000001"

const (
	// sessionTokenTTL is the sliding window a session stays valid without use.
	sessionTokenTTL = 30 * 24 * time.Hour
	// sessionTokenMaxAge is the ceiling sliding cannot pass: a session signs
	// in again after this, however busy it was.
	sessionTokenMaxAge = 90 * 24 * time.Hour
	// tokenSlideEvery caps how often one token's use rewrites its row — the
	// write budget is what the throttle protects, not correctness.
	tokenSlideEvery = time.Hour
)

// ErrLastAdmin refuses the change that would leave no enabled admin.
var ErrLastAdmin = errors.New("that would leave no admin")

// ErrDisabled is a login by an account an admin switched off.
var ErrDisabled = errors.New("account disabled")

// Token prefixes make a leaked string identifiable in a log or a paste
// without revealing anything: the secret part follows the prefix.
const (
	sessionTokenPrefix = "ags_s_"
	patTokenPrefix     = "ags_p_"
)

func newTokenSecret(kind string) string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	s := base64.RawURLEncoding.EncodeToString(b)
	if kind == TokenKindPAT {
		return patTokenPrefix + s
	}
	return sessionTokenPrefix + s
}

// HashToken is the stored form of a token: SHA-256 hex. The plaintext is never
// written anywhere; possession of the database does not grant possession of a
// credential.
func HashToken(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

// UserStore persists accounts and their OAuth identities.
type UserStore struct{ db *bun.DB }

// NewUserStore returns a UserStore backed by db.
func NewUserStore(db *bun.DB) *UserStore { return &UserStore{db: db} }

// EnsureLocalUser upserts the implicit token-mode account and returns it.
func (s *UserStore) EnsureLocalUser(ctx context.Context) (*User, error) {
	u := &User{ID: LocalUserID, Email: "local@localhost", Name: "Local", Role: RoleAdmin}
	if _, err := s.db.NewInsert().Model(u).On("CONFLICT (id) DO NOTHING").Exec(ctx); err != nil {
		return nil, fmt.Errorf("ensuring the local user: %w", err)
	}
	return s.ByID(ctx, LocalUserID)
}

// ByID returns one user, ErrNotFound when absent.
func (s *UserStore) ByID(ctx context.Context, id string) (*User, error) {
	u := new(User)
	if err := s.db.NewSelect().Model(u).Where("id = ?", id).Scan(ctx); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("user %s: %w", id, ErrNotFound)
		}
		return nil, err
	}
	return u, nil
}

// List returns every account, oldest first — the admin's user management view.
func (s *UserStore) List(ctx context.Context) ([]User, error) {
	var out []User
	if err := s.db.NewSelect().Model(&out).OrderExpr("created_at ASC").Scan(ctx); err != nil {
		return nil, fmt.Errorf("listing users: %w", err)
	}
	return out, nil
}

// CountReal counts the accounts people sign in as — every user but the
// token-mode local one.
func (s *UserStore) CountReal(ctx context.Context) (int, error) {
	n, err := s.db.NewSelect().Model((*User)(nil)).Where("id != ?", LocalUserID).Count(ctx)
	if err != nil {
		return 0, fmt.Errorf("counting users: %w", err)
	}
	return n, nil
}

// UserPatch is what an admin may change about an account; a nil field is
// left alone.
type UserPatch struct {
	Role     *string
	Disabled *bool
}

// Patch applies p to one account, in a transaction that refuses to leave no
// enabled admin (ErrLastAdmin) — the local account, which cannot sign in,
// does not count as one. ErrNotFound when absent.
func (s *UserStore) Patch(ctx context.Context, id string, p UserPatch) error {
	return s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		u := new(User)
		if err := tx.NewSelect().Model(u).Where("id = ?", id).Scan(ctx); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if p.Role != nil {
			u.Role = *p.Role
		}
		if p.Disabled != nil {
			if *p.Disabled && u.DisabledAt.IsZero() {
				u.DisabledAt = time.Now().UTC()
			} else if !*p.Disabled {
				u.DisabledAt = time.Time{}
			}
		}
		if u.Role != RoleAdmin || !u.DisabledAt.IsZero() {
			n, err := tx.NewSelect().Model((*User)(nil)).
				Where("id != ? AND id != ? AND role = ? AND disabled_at IS NULL", id, LocalUserID, RoleAdmin).
				Count(ctx)
			if err != nil {
				return err
			}
			if n == 0 {
				return ErrLastAdmin
			}
		}
		u.UpdatedAt = time.Now().UTC()
		_, err := tx.NewUpdate().Model(u).Column("role", "disabled_at", "updated_at").Where("id = ?", id).Exec(ctx)
		return err
	})
}

// OAuthIdentity is what a login provider reports about the person after a
// completed code exchange. Email must be one the provider verified.
type OAuthIdentity struct {
	Provider  string
	Subject   string
	Email     string
	Name      string
	AvatarURL string
}

// ResolveOAuthLogin finds or creates the account for one completed OAuth login.
// Merge order: the (provider, subject) identity wins; else a user with the
// same verified email gains a new identity (one person, several providers);
// else a new account. A login matching bootstrapAdmin is an admin; with no
// bootstrapAdmin the first OAuth account is (README "OAuth mode"). Concurrent
// logins of one person are arbitrated by the unique indexes — the loser's
// transaction fails and its one retry sees the winner's rows; concurrent
// first logins of two people are serialized by firstAccountLock.
func (s *UserStore) ResolveOAuthLogin(ctx context.Context, id OAuthIdentity, bootstrapAdmin string) (*User, error) {
	u, err := s.resolveOAuthLogin(ctx, id, bootstrapAdmin)
	if _, dup := UniqueViolation(err); dup {
		u, err = s.resolveOAuthLogin(ctx, id, bootstrapAdmin)
	}
	return u, err
}

func (s *UserStore) resolveOAuthLogin(ctx context.Context, id OAuthIdentity, bootstrapAdmin string) (*User, error) {
	email := strings.ToLower(strings.TrimSpace(id.Email))
	bootstrapAdmin = strings.ToLower(strings.TrimSpace(bootstrapAdmin))
	if email == "" {
		return nil, errors.New("the provider reported no verified email")
	}
	var out *User
	err := s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		u, err := userForIdentity(ctx, tx, id, email, bootstrapAdmin == "")
		if err != nil {
			return err
		}
		if !u.DisabledAt.IsZero() {
			return ErrDisabled
		}
		// The provider's view of the person refreshes on every login; the
		// email does not — it is the merge key, and identity follows subject.
		u.Name = id.Name
		u.AvatarURL = id.AvatarURL
		u.LastLoginAt = time.Now().UTC()
		if email == bootstrapAdmin {
			u.Role = RoleAdmin
		}
		if _, err := tx.NewUpdate().Model(u).
			Column("name", "avatar_url", "last_login_at", "role", "updated_at").
			Where("id = ?", u.ID).Exec(ctx); err != nil {
			return err
		}
		out = u
		return nil
	})
	return out, err
}

// userForIdentity resolves the account inside the login transaction, creating
// rows as the merge order requires. firstIsAdmin makes a brand-new account the
// admin when it is the first real one.
func userForIdentity(ctx context.Context, tx bun.Tx, id OAuthIdentity, email string, firstIsAdmin bool) (*User, error) {
	idn := new(Identity)
	err := tx.NewSelect().Model(idn).
		Where("provider = ? AND subject = ?", id.Provider, id.Subject).Scan(ctx)
	if err == nil {
		u := new(User)
		if err := tx.NewSelect().Model(u).Where("id = ?", idn.UserID).Scan(ctx); err != nil {
			return nil, err
		}
		return u, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}

	u := new(User)
	err = tx.NewSelect().Model(u).Where("email = ?", email).Scan(ctx)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		u = &User{Email: email, Role: RoleMember}
		if firstIsAdmin {
			// Two different people's first logins must not both count zero:
			// PostgreSQL runs READ COMMITTED, so the count is serialized by a
			// transaction-scoped advisory lock (SQLite's one connection
			// serializes by itself).
			if tx.Dialect().Name() == dialect.PG {
				if _, err := tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock(?)", firstAccountLock); err != nil {
					return nil, err
				}
			}
			n, cerr := tx.NewSelect().Model((*User)(nil)).Where("id != ?", LocalUserID).Count(ctx)
			if cerr != nil {
				return nil, cerr
			}
			if n == 0 {
				u.Role = RoleAdmin
			}
		}
		if _, err := tx.NewInsert().Model(u).Exec(ctx); err != nil {
			return nil, err
		}
	case err != nil:
		return nil, err
	}
	link := &Identity{UserID: u.ID, Provider: id.Provider, Subject: id.Subject}
	if _, err := tx.NewInsert().Model(link).Exec(ctx); err != nil {
		return nil, err
	}
	return u, nil
}

// firstAccountLock keys the advisory lock that serializes "is this the first
// account" on PostgreSQL.
const firstAccountLock = 0x6167656e7473 // "agents"

// AuthTokenStore persists session tokens and PATs, hashed.
type AuthTokenStore struct{ db *bun.DB }

// NewAuthTokenStore returns an AuthTokenStore backed by db.
func NewAuthTokenStore(db *bun.DB) *AuthTokenStore { return &AuthTokenStore{db: db} }

// Mint creates a token and returns its plaintext — the only moment it exists.
// A session gets the sliding TTL; a PAT keeps expiresAt as given (zero = never).
func (s *AuthTokenStore) Mint(ctx context.Context, userID, kind, name string, expiresAt time.Time) (string, *AuthToken, error) {
	secret := newTokenSecret(kind)
	t := &AuthToken{UserID: userID, Kind: kind, TokenHash: HashToken(secret), Name: name, ExpiresAt: expiresAt}
	if kind == TokenKindSession {
		t.ExpiresAt = time.Now().UTC().Truncate(time.Microsecond).Add(sessionTokenTTL)
	}
	if _, err := s.db.NewInsert().Model(t).Exec(ctx); err != nil {
		return "", nil, fmt.Errorf("minting a %s token: %w", kind, err)
	}
	return secret, t, nil
}

// Authenticate resolves a presented plaintext to its user and token row, or
// ErrNotFound — expired counts as absent (and is deleted in passing). A hit
// slides a session's expiry and stamps last_used_at, at most once per
// tokenSlideEvery.
func (s *AuthTokenStore) Authenticate(ctx context.Context, plaintext string) (*User, *AuthToken, error) {
	// Microseconds is the column precision; the returned token must equal the row.
	now := time.Now().UTC().Truncate(time.Microsecond)
	t := new(AuthToken)
	if err := s.db.NewSelect().Model(t).Where("token_hash = ?", HashToken(plaintext)).Scan(ctx); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil, ErrNotFound
		}
		return nil, nil, err
	}
	expired := !t.ExpiresAt.IsZero() && now.After(t.ExpiresAt)
	if t.Kind == TokenKindSession && now.Sub(t.CreatedAt) > sessionTokenMaxAge {
		expired = true
	}
	if expired {
		_, _ = s.db.NewDelete().Model((*AuthToken)(nil)).Where("id = ?", t.ID).Exec(ctx)
		return nil, nil, ErrNotFound
	}
	if t.LastUsedAt.IsZero() || now.Sub(t.LastUsedAt) > tokenSlideEvery {
		t.LastUsedAt = now
		cols := []string{"last_used_at", "updated_at"}
		if t.Kind == TokenKindSession {
			t.ExpiresAt = now.Add(sessionTokenTTL)
			cols = append(cols, "expires_at")
		}
		if _, err := s.db.NewUpdate().Model(t).Column(cols...).Where("id = ?", t.ID).Exec(ctx); err != nil {
			return nil, nil, err
		}
	}
	u := new(User)
	if err := s.db.NewSelect().Model(u).Where("id = ?", t.UserID).Scan(ctx); err != nil {
		return nil, nil, fmt.Errorf("user for token: %w", err)
	}
	if !u.DisabledAt.IsZero() {
		return nil, nil, ErrNotFound
	}
	return u, t, nil
}

// RevokeAllForUser deletes every token — sessions and PATs — of one user:
// the admin's "sign them out everywhere".
func (s *AuthTokenStore) RevokeAllForUser(ctx context.Context, userID string) (int64, error) {
	res, err := s.db.NewDelete().Model((*AuthToken)(nil)).Where("user_id = ?", userID).Exec(ctx)
	if err != nil {
		return 0, fmt.Errorf("revoking tokens of user %s: %w", userID, err)
	}
	return res.RowsAffected()
}

// ListByUser returns one user's tokens of the given kind, newest first.
func (s *AuthTokenStore) ListByUser(ctx context.Context, userID, kind string) ([]AuthToken, error) {
	var out []AuthToken
	err := s.db.NewSelect().Model(&out).
		Where("user_id = ? AND kind = ?", userID, kind).
		Order("created_at DESC").Scan(ctx)
	return out, err
}

// Revoke deletes one token the user owns; ErrNotFound covers both a wrong id
// and someone else's token, indistinguishably.
func (s *AuthTokenStore) Revoke(ctx context.Context, id, userID string) error {
	res, err := s.db.NewDelete().Model((*AuthToken)(nil)).
		Where("id = ? AND user_id = ?", id, userID).Exec(ctx)
	if err != nil {
		return err
	}
	return requireRows(res)
}

// RevokeByPlaintext deletes the presented token — logout.
func (s *AuthTokenStore) RevokeByPlaintext(ctx context.Context, plaintext string) error {
	res, err := s.db.NewDelete().Model((*AuthToken)(nil)).
		Where("token_hash = ?", HashToken(plaintext)).Exec(ctx)
	if err != nil {
		return err
	}
	return requireRows(res)
}

// DeleteExpired removes every token past its expiry; the maintenance loop's
// half of the lazy cleanup Authenticate does in passing.
func (s *AuthTokenStore) DeleteExpired(ctx context.Context) (int64, error) {
	res, err := s.db.NewDelete().Model((*AuthToken)(nil)).
		Where("expires_at IS NOT NULL AND expires_at < ?", time.Now().UTC()).Exec(ctx)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}
