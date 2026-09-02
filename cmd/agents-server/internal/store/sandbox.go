package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect"

	e2bsb "github.com/zzir/agents-go/sandbox/e2b"
)

// This file is the single home of sandbox semantics: what a payload must
// contain to be storable (NormalizeSandboxConfig), when two payloads mean the
// same runtime content (SandboxContentEqual), and when an update moves the
// sandbox's identity (SandboxIdentityChanged). Every other per-type question
// — capabilities, freeze messages, storage hints — is a sandboxKinds
// row, so a new backend is one entry here plus its sandboxes.Backend.

// sandboxKind is one backend type's semantics. Each field answers exactly one
// question; the exported functions below route through it.
type sandboxKind struct {
	contentEqual func(a, b json.RawMessage) bool           // see SandboxContentEqual
	destination  func(raw json.RawMessage) (string, error) // see destinationOf
	identity     func(raw json.RawMessage) (string, error) // see identityOf
	// frozenFields finishes the identity-conflict 409: what freezes while
	// projects live on the sandbox, and what stays editable.
	frozenFields string
	// storageWhere is the per-sandbox half of a storage hint (see
	// SandboxStorageWhere).
	storageWhere func(raw json.RawMessage) string
	supports     SandboxSupports
}

// SandboxSupports is a type's capability row, carried on every sandbox the
// API returns — derived from the type, read-only.
type SandboxSupports struct {
	// Rebuild: the compute can be thrown away in place, keeping the storage.
	Rebuild bool `json:"rebuild"`
}

var sandboxKinds = map[string]sandboxKind{
	"docker": {
		contentEqual: func(a, b json.RawMessage) bool { return canonicalEqual(a, b, func(*DockerConfig) {}) },
		destination:  dockerDestination,
		identity:     dockerDestination, // nothing beyond the destination freezes
		frozenFields: "its type and machine are frozen — the image, the limits, the credential and the name stay editable",
		storageWhere: dockerStorageWhere,
		supports:     SandboxSupports{Rebuild: true},
	},
	"e2b": {
		contentEqual: func(a, b json.RawMessage) bool { return canonicalEqual(a, b, func(*E2BConfig) {}) },
		destination:  e2bDestination,
		identity:     e2bIdentity,
		frozenFields: "its type, service address, template and lifecycle (auto-pause, internet) are frozen — the api key, timeout, read limit and name stay editable",
		storageWhere: e2bStorageWhere,
		supports:     SandboxSupports{},
	},
}

// kindOf resolves a type's descriptor. An unknown type panics:
// NormalizeSandboxConfig refuses to store one, so reaching here with it is a
// programming error a quiet per-type default would answer wrongly.
func kindOf(typ string) sandboxKind {
	k, ok := sandboxKinds[typ]
	if !ok {
		panic(fmt.Sprintf("store: unknown sandbox type %q", typ))
	}
	return k
}

// SandboxTypes are the backends a sandbox may name — the descriptor map's
// keys, in stable order.
var SandboxTypes = slices.Sorted(maps.Keys(sandboxKinds))

// SandboxSupportsFor is the capability row for a type.
func SandboxSupportsFor(typ string) SandboxSupports {
	return kindOf(typ).supports
}

// SandboxFrozenFields names, for the identity-conflict refusal, what freezes
// on this type while projects live on the sandbox and what stays editable.
func SandboxFrozenFields(typ string) string {
	return kindOf(typ).frozenFields
}

// SandboxStorageWhere names WHERE a project's files live on this sandbox: the
// daemon address for docker, "sandbox on <service>" where the instance IS the
// storage. An undecodable config falls back to the type's zero config.
func SandboxStorageWhere(sb *Sandbox) string {
	return kindOf(sb.Type).storageWhere(sb.Config)
}

// NormalizeSandboxConfig strictly decodes raw and returns the canonical
// payload to store. It gates the API write path: a payload that decodes here
// builds later — a stored type mismatch would otherwise read as its zero value
// at build time. Canonical means fields re-marshaled in struct order and
// unknown keys dropped (nothing consumes them).
func NormalizeSandboxConfig(typ string, raw json.RawMessage) (json.RawMessage, error) {
	switch typ {
	case "docker":
		var dc DockerConfig
		if err := DecodeConfig(raw, &dc); err != nil {
			return nil, fmt.Errorf("docker sandbox config: %w", err)
		}
		switch {
		case dc.Host == "" || strings.HasPrefix(dc.Host, "tcp://"):
		case strings.HasPrefix(dc.Host, "ssh://"):
			if !strings.Contains(strings.TrimPrefix(dc.Host, "ssh://"), "@") {
				return nil, errors.New("an ssh:// host must carry its user: ssh://user@host")
			}
		default:
			return nil, errors.New("host must be empty (local daemon), tcp://host:port, or ssh://user@host")
		}
		if dc.Image == "" {
			return nil, errors.New("a docker sandbox requires config.image")
		}
		if dc.MaxReadFileBytes < 0 {
			return nil, errors.New("max_read_file_bytes cannot be negative")
		}
		if dc.MemoryMB < 0 || dc.CPUs < 0 {
			return nil, errors.New("memory_mb and cpus cannot be negative")
		}
		return json.Marshal(dc)
	case "e2b":
		var ec E2BConfig
		if err := DecodeConfig(raw, &ec); err != nil {
			return nil, fmt.Errorf("e2b sandbox config: %w", err)
		}
		if ec.APIURL != "" && !strings.HasPrefix(ec.APIURL, "http://") && !strings.HasPrefix(ec.APIURL, "https://") {
			return nil, errors.New("api_url must be an absolute http(s) URL")
		}
		switch ec.DataPlaneAuth {
		case "", "access_token", "api_key", "none":
		default:
			return nil, errors.New(`data_plane_auth must be "", "access_token", "api_key" or "none"`)
		}
		if ec.TemplateID == "" {
			return nil, errors.New("an e2b sandbox requires config.template_id — build it on the service first")
		}
		if ec.TimeoutSeconds < 0 || ec.MaxReadFileBytes < 0 {
			return nil, errors.New("timeout_seconds and max_read_file_bytes cannot be negative")
		}
		// auto_pause defaults to true — pause on expiry, keeping the working
		// tree — when the field is absent; an explicit false is kill-on-expiry.
		// A plain bool cannot tell absent from false, so detect the key and make
		// the stored form explicit (the tag drops omitempty for the same reason).
		if !jsonHasKey(raw, "auto_pause") {
			ec.AutoPause = true
		}
		return json.Marshal(ec)
	default:
		return nil, fmt.Errorf("sandbox type must be one of %s, got %q", strings.Join(SandboxTypes, ", "), typ)
	}
}

// SandboxContentEqual reports whether two payloads mean the same runtime
// CONTENT — the predicate behind contentChanged (the projects'
// runtime-generation bump, and the instance retirement that follows).
// Canonical typed comparison keeps representation noise — omitted-vs-zero
// fields, unknown keys — from tearing down a container; a payload that cannot
// decode compares UNEQUAL, the safe side.
func SandboxContentEqual(typ string, a, b json.RawMessage) bool {
	return kindOf(typ).contentEqual(a, b)
}

// SandboxIdentityChanged reports whether an update moves the sandbox's
// IDENTITY — the fields that freeze while projects live on it (decisions
// §5.36): the type and destination for every backend, plus, for e2b, the
// fields a /connect resume cannot apply to an already-provisioned sandbox
// (template_id, auto_pause, allow_internet). Freezing them refuses an edit
// that would otherwise look saved yet silently never take effect; timeout is
// NOT among them — resume re-sends it, so it propagates on the next build.
// An undecodable prev is NOT a change — fixing it is a referenced sandbox's
// only way out; an undecodable next counts as one, pure defense.
func SandboxIdentityChanged(prev, next *Sandbox) bool {
	if prev.Type != next.Type {
		return true
	}
	p, perr := identityOf(prev.Type, prev.Config)
	n, nerr := identityOf(next.Type, next.Config)
	if perr != nil {
		return false
	}
	if nerr != nil {
		return true
	}
	return p != n
}

// SandboxDestinationChanged reports whether incoming names a different
// DESTINATION than prev — the address a stored credential would ride to. The
// mask guard uses it: a credential sent back masked means "keep the stored
// one", which holds only while the destination is unchanged. Either side
// undecodable counts as changed — the safe side (refuse the masked carry-over).
func SandboxDestinationChanged(typ string, prev, incoming json.RawMessage) bool {
	p, perr := destinationOf(typ, prev)
	n, nerr := destinationOf(typ, incoming)
	if perr != nil || nerr != nil {
		return true
	}
	return p != n
}

// destinationOf is the address a project's files — and a data-plane credential
// — live at: the daemon host for docker, the control plane AND the public-host
// domain for e2b (both, so a domain-only move is still a move).
func destinationOf(typ string, raw json.RawMessage) (string, error) {
	return kindOf(typ).destination(raw)
}

// identityOf is the destination plus, for e2b, the fields a resume cannot
// change on an existing sandbox — so a referenced sandbox freezes them rather
// than accept an edit that silently never applies.
func identityOf(typ string, raw json.RawMessage) (string, error) {
	return kindOf(typ).identity(raw)
}

func dockerDestination(raw json.RawMessage) (string, error) {
	var dc DockerConfig
	if err := DecodeConfig(raw, &dc); err != nil {
		return "", err
	}
	return jsonKey(dc.Host), nil
}

func e2bDestination(raw json.RawMessage) (string, error) {
	var ec E2BConfig
	if err := DecodeConfig(raw, &ec); err != nil {
		return "", err
	}
	return jsonKey(ec.APIURL, ec.Domain), nil
}

func e2bIdentity(raw json.RawMessage) (string, error) {
	var ec E2BConfig
	if err := DecodeConfig(raw, &ec); err != nil {
		return "", err
	}
	return jsonKey(ec.APIURL, ec.Domain, ec.TemplateID, ec.AutoPause, ec.AllowInternet), nil
}

// The storageWhere pair ignores decode errors deliberately: a hint is
// best-effort and the zero config still names the right service.
func dockerStorageWhere(raw json.RawMessage) string {
	var dc DockerConfig
	_ = json.Unmarshal(raw, &dc)
	if dc.Host == "" {
		return "the local daemon"
	}
	return dc.Host
}

func e2bStorageWhere(raw json.RawMessage) string {
	var ec E2BConfig
	_ = json.Unmarshal(raw, &ec)
	host := ec.APIURL
	if host == "" {
		host = e2bsb.DefaultAPIURL
	}
	return "sandbox on " + host
}

// jsonKey renders fields as a self-delimiting equality key: JSON quotes and
// escapes every value, so a field that contains the separator cannot collide
// with the next field the way a plain "a|b" join can.
func jsonKey(fields ...any) string {
	b, _ := json.Marshal(fields)
	return string(b)
}

// sandboxDestination is destinationOf for a stored row — checkMove's
// same-machine guard on a project move.
func sandboxDestination(s *Sandbox) (string, error) {
	return destinationOf(s.Type, s.Config)
}

// SandboxStore persists sandboxes.
type SandboxStore struct {
	*CrudStore[Sandbox]
	db *bun.DB
}

// NewSandboxStore returns a store backed by db.
func NewSandboxStore(db *bun.DB) *SandboxStore {
	return &SandboxStore{
		CrudStore: NewCrudStore[Sandbox](db, "sandbox", "created_at DESC").withSecrets(sealSandbox, openSandbox),
		db:        db,
	}
}

// Create inserts the sandbox at revision 1 — the counter every later write
// maintains.
func (s *SandboxStore) Create(ctx context.Context, sb *Sandbox) error {
	if sb.Revision == 0 {
		sb.Revision = 1
	}
	return s.CrudStore.Create(ctx, sb)
}

// noProjectsOnSandbox is the guard the identity update and the delete share:
// no project's tree lives on this sandbox. In the statement's WHERE clause it
// is atomic only under SQLite's single writer; the PostgreSQL paths instead
// lock the sandbox row FOR UPDATE and re-read the guard (pgGuardSandbox).
const noProjectsOnSandbox = "NOT EXISTS (SELECT 1 FROM projects WHERE sandbox_id = ?)"

// pgGuardSandbox locks the sandbox row FOR UPDATE — the lock project
// Create/Update take on it — and returns its revision and how many projects
// live on it, both read fresh under the lock. ErrNotFound when the row is gone.
func pgGuardSandbox(ctx context.Context, tx bun.Tx, id string) (revision int64, projects int, err error) {
	err = tx.NewSelect().Model((*Sandbox)(nil)).Column("revision").
		Where("id = ?", id).For("UPDATE").Scan(ctx, &revision)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, ErrNotFound
	}
	if err != nil {
		return 0, 0, err
	}
	projects, err = tx.NewSelect().Model((*Project)(nil)).Where("sandbox_id = ?", id).Count(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("counting projects on sandbox %s: %w", id, err)
	}
	return revision, projects, nil
}

// Update overwrites the sandbox, shadowing the generic CrudStore update with
// the revision counter and a compare-and-set: the write lands only while the
// row is still at expectedRevision (see ErrRevisionConflict). Retiring what
// runs is the caller's next act, on the projects (ProjectStore.BumpRuntimeGen).
func (s *SandboxStore) Update(ctx context.Context, id string, sb *Sandbox, expectedRevision int64) error {
	sb.ID = id
	var res sql.Result
	err := sealedWrite(sb, sealSandbox, openSandbox, func() (err error) {
		res, err = s.db.NewUpdate().Model(sb).
			ExcludeColumn("id", "created_at", "revision").
			Set("revision = revision + 1").
			Where("id = ?", id).
			Where("revision = ?", expectedRevision).
			Exec(ctx)
		return err
	})
	if err != nil {
		return fmt.Errorf("updating sandbox %s: %w", id, err)
	}
	if n, aerr := res.RowsAffected(); aerr == nil && n > 0 {
		return nil
	}
	return s.explainWriteRefusal(ctx, id)
}

// UpdateIdentityIfUnreferenced overwrites the sandbox only while no project
// lives on it AND the row is still at expectedRevision — the write path for
// updates that move the sandbox's IDENTITY (see SandboxIdentityChanged). A
// referenced sandbox refuses with the blocking count; a moved revision is
// ErrRevisionConflict.
func (s *SandboxStore) UpdateIdentityIfUnreferenced(ctx context.Context, id string, sb *Sandbox, expectedRevision int64) (projects int, err error) {
	sb.ID = id
	if s.db.Dialect().Name() == dialect.PG {
		err = sealedWrite(sb, sealSandbox, openSandbox, func() error {
			return s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
				revision, n, gerr := pgGuardSandbox(ctx, tx, id)
				if gerr != nil {
					return gerr
				}
				if revision != expectedRevision {
					return ErrRevisionConflict
				}
				if n > 0 {
					projects = n
					return nil
				}
				_, uerr := tx.NewUpdate().Model(sb).
					ExcludeColumn("id", "created_at", "revision").
					Set("revision = revision + 1").
					Where("id = ?", id).
					Exec(ctx)
				return uerr
			})
		})
		if err != nil && !errors.Is(err, ErrRevisionConflict) {
			err = fmt.Errorf("updating sandbox %s: %w", id, err)
		}
		return projects, err
	}
	var res sql.Result
	err = sealedWrite(sb, sealSandbox, openSandbox, func() (err error) {
		res, err = s.db.NewUpdate().Model(sb).
			ExcludeColumn("id", "created_at", "revision").
			Set("revision = revision + 1").
			Where("id = ?", id).
			Where("revision = ?", expectedRevision).
			Where(noProjectsOnSandbox, id).
			Exec(ctx)
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("updating sandbox %s: %w", id, err)
	}
	if n, aerr := res.RowsAffected(); aerr == nil && n > 0 {
		return 0, nil
	}
	cur, gerr := s.Get(ctx, id)
	if gerr != nil {
		return 0, gerr // ErrNotFound included
	}
	if cur.Revision != expectedRevision {
		return 0, ErrRevisionConflict
	}
	return s.countBlockers(ctx, id)
}

// DeleteIfUnreferenced deletes the sandbox only while no project lives on it —
// atomic under SQLite's single writer; on PostgreSQL it locks the row FOR
// UPDATE (the lock a project create takes) and re-reads the guard, so a
// project create cannot land between the reference count and the delete. It
// returns how many projects blocked the delete: 0 with a nil error means
// deleted; >0 means refused. A missing sandbox is ErrNotFound — a different
// answer from refused, and the handler maps them to 404 vs 409.
//
// There is no cascade: a project delete reclaims its storage
// (decisions §5.33), so taking projects along would destroy working trees as a
// side effect of removing a machine.
func (s *SandboxStore) DeleteIfUnreferenced(ctx context.Context, id string) (projects int, err error) {
	if s.db.Dialect().Name() == dialect.PG {
		err = s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
			_, n, gerr := pgGuardSandbox(ctx, tx, id)
			if gerr != nil {
				return gerr
			}
			if n > 0 {
				projects = n
				return nil
			}
			if _, derr := tx.NewDelete().Model((*Sandbox)(nil)).Where("id = ?", id).Exec(ctx); derr != nil {
				return fmt.Errorf("deleting sandbox %s: %w", id, derr)
			}
			return nil
		})
		return projects, err
	}
	res, err := s.db.NewDelete().Model((*Sandbox)(nil)).
		Where("id = ?", id).
		Where(noProjectsOnSandbox, id).
		Exec(ctx)
	if err != nil {
		return 0, fmt.Errorf("deleting sandbox %s: %w", id, err)
	}
	if n, aerr := res.RowsAffected(); aerr == nil && n > 0 {
		return 0, nil
	}
	return s.countBlockers(ctx, id)
}

// explainWriteRefusal disambiguates a zero-row conditional write that carried
// no reference guard: the row is missing, or its revision moved.
func (s *SandboxStore) explainWriteRefusal(ctx context.Context, id string) error {
	exists, err := s.db.NewSelect().Model((*Sandbox)(nil)).Where("id = ?", id).Exists(ctx)
	if err != nil {
		return fmt.Errorf("checking sandbox %s: %w", id, err)
	}
	if !exists {
		return fmt.Errorf("updating sandbox %s: %w", id, ErrNotFound)
	}
	return ErrRevisionConflict
}

// countBlockers reports how many projects hold the sandbox down, or
// ErrNotFound when it is gone. The reference could have vanished between the
// write and this read; it reports at least one so the caller still refuses
// rather than inventing success.
func (s *SandboxStore) countBlockers(ctx context.Context, id string) (int, error) {
	exists, err := s.db.NewSelect().Model((*Sandbox)(nil)).Where("id = ?", id).Exists(ctx)
	if err != nil {
		return 0, fmt.Errorf("checking sandbox %s: %w", id, err)
	}
	if !exists {
		return 0, ErrNotFound
	}
	n, err := s.db.NewSelect().Model((*Project)(nil)).Where("sandbox_id = ?", id).Count(ctx)
	if err != nil {
		return 0, fmt.Errorf("counting projects on sandbox %s: %w", id, err)
	}
	return max(n, 1), nil
}

// canonicalEqual compares two raw payloads through T, canonicalized. The
// comparable constraint is deliberate: it keeps every field of the config
// struct a plain value, so adding a slice or map field breaks the build
// here and forces a decision about how it compares instead of silently
// changing semantics.
func canonicalEqual[T comparable](a, b json.RawMessage, canon func(*T)) bool {
	var va, vb T
	if DecodeConfig(a, &va) != nil || DecodeConfig(b, &vb) != nil {
		return false
	}
	canon(&va)
	canon(&vb)
	return va == vb
}

// jsonHasKey reports whether the top-level JSON object in raw carries key —
// how a normalizer tells an absent field from an explicit zero value.
func jsonHasKey(raw json.RawMessage, key string) bool {
	if len(raw) == 0 {
		return false
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return false
	}
	_, ok := m[key]
	return ok
}
