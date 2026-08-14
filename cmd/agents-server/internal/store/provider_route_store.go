package store

import (
	"context"
	"fmt"

	"github.com/uptrace/bun"
)

// ProviderRouteStore persists provider routes.
type ProviderRouteStore struct {
	*CrudStore[ProviderRoute]
	db *bun.DB
}

// NewProviderRouteStore returns a ProviderRouteStore backed by db. Prefix
// uniqueness is enforced by the DB (idx_provider_routes_prefix); a duplicate
// surfaces as a UNIQUE-constraint error that handlers map to 409.
func NewProviderRouteStore(db *bun.DB) *ProviderRouteStore {
	return &ProviderRouteStore{CrudStore: NewCrudStore[ProviderRoute](db, "provider route", "prefix ASC"), db: db}
}

// Create writes the route only if its provider still exists AND is routable,
// atomically — shadowing the embedded CrudStore.Create so the handler's checks
// and this write cannot straddle a provider delete or an auth-mode switch
// (ErrProviderRef / ErrProviderNotRoutable if they do).
func (s *ProviderRouteStore) Create(ctx context.Context, pr *ProviderRoute) error {
	return writeReferencingProvider(ctx, s.db, pr.ProviderID, routableProvider, func(ctx context.Context, tx bun.Tx) error {
		_, err := tx.NewInsert().Model(pr).Exec(ctx)
		return err
	})
}

// Update overwrites the route under the same guard as Create: re-pointing a
// route races a provider delete or auth-mode switch exactly like creating one.
func (s *ProviderRouteStore) Update(ctx context.Context, id string, pr *ProviderRoute) error {
	err := writeReferencingProvider(ctx, s.db, pr.ProviderID, routableProvider, func(ctx context.Context, tx bun.Tx) error {
		res, err := tx.NewUpdate().Model(pr).
			ExcludeColumn("id", "created_at").
			Where("id = ?", id).
			Exec(ctx)
		if err == nil {
			err = requireRows(res)
		}
		return err
	})
	if err != nil {
		return fmt.Errorf("updating provider route %s: %w", id, err)
	}
	return nil
}
