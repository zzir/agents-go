package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/uptrace/bun"
)

// AttachmentScheme prefixes an attachment id in the image_url a session
// entry stores: "agents-attachment:<id>", resolved at the model boundary — invariant 56.
const AttachmentScheme = "agents-attachment:"

// AttachmentSentinelURL returns the image_url an entry stores for id.
func AttachmentSentinelURL(id string) string { return AttachmentScheme + id }

// AttachmentSentinelID returns the id inside a sentinel URL, or "" for any
// other URL.
func AttachmentSentinelID(u string) string {
	if rest, ok := strings.CutPrefix(u, AttachmentScheme); ok {
		return rest
	}
	return ""
}

// AttachmentStore persists uploaded-image metadata; the bytes are in the
// bucket, and the model boundary resolves these rows to public URLs.
type AttachmentStore struct {
	db *bun.DB
}

// NewAttachmentStore returns an AttachmentStore backed by db.
func NewAttachmentStore(db *bun.DB) *AttachmentStore {
	return &AttachmentStore{db: db}
}

// Create inserts the row, minting its id and timestamp.
func (s *AttachmentStore) Create(ctx context.Context, a *Attachment) error {
	if a.ID == "" {
		a.ID = NewID()
	}
	a.CreatedAt = time.Now().UTC()
	if _, err := s.db.NewInsert().Model(a).Exec(ctx); err != nil {
		return fmt.Errorf("creating attachment: %w", err)
	}
	return nil
}

// Get returns one attachment, or an ErrNotFound-wrapping error.
func (s *AttachmentStore) Get(ctx context.Context, id string) (*Attachment, error) {
	a := new(Attachment)
	err := s.db.NewSelect().Model(a).Where("id = ?", id).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("attachment %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("loading attachment %s: %w", id, err)
	}
	return a, nil
}

// MetaBatch returns the named attachments keyed by id. Missing ids are simply
// absent from the map — the caller decides whether absence degrades or fails.
func (s *AttachmentStore) MetaBatch(ctx context.Context, ids []string) (map[string]Attachment, error) {
	out := make(map[string]Attachment, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	var rows []Attachment
	if err := s.db.NewSelect().Model(&rows).Where("id IN (?)", bun.List(ids)).Scan(ctx); err != nil {
		return nil, fmt.Errorf("loading attachments: %w", err)
	}
	for _, a := range rows {
		out[a.ID] = a
	}
	return out, nil
}

// MarkBound flips the named rows to bound. Idempotent: rebinding an already
// bound attachment (the same image sent in a second message) changes nothing.
func (s *AttachmentStore) MarkBound(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	if _, err := s.db.NewUpdate().Model((*Attachment)(nil)).
		Set("bound = ?", true).
		Where("id IN (?)", bun.List(ids)).Exec(ctx); err != nil {
		return fmt.Errorf("binding attachments: %w", err)
	}
	return nil
}

// ListUnboundBefore returns rows never accepted by a run and older than
// cutoff — the orphans the reaper collects.
func (s *AttachmentStore) ListUnboundBefore(ctx context.Context, cutoff time.Time) ([]Attachment, error) {
	var rows []Attachment
	if err := s.db.NewSelect().Model(&rows).
		Where("bound = ?", false).
		Where("created_at < ?", cutoff).Scan(ctx); err != nil {
		return nil, fmt.Errorf("listing orphan attachments: %w", err)
	}
	return rows, nil
}

// Delete removes the row. Deleting an absent id is not an error: a reaper
// retry may find the row already gone.
func (s *AttachmentStore) Delete(ctx context.Context, id string) error {
	if _, err := s.db.NewDelete().Model((*Attachment)(nil)).Where("id = ?", id).Exec(ctx); err != nil {
		return fmt.Errorf("deleting attachment %s: %w", id, err)
	}
	return nil
}
