package store

import "github.com/uptrace/bun"

// ProviderRouteStore persists provider routes.
type ProviderRouteStore struct {
	*CrudStore[ProviderRoute]
}

// NewProviderRouteStore returns a ProviderRouteStore backed by db. Prefix
// uniqueness is enforced by the DB (idx_provider_routes_prefix); a duplicate
// surfaces as a UNIQUE-constraint error that handlers map to 409.
func NewProviderRouteStore(db *bun.DB) *ProviderRouteStore {
	return &ProviderRouteStore{NewCrudStore[ProviderRoute](db, "provider route", "prefix ASC")}
}
