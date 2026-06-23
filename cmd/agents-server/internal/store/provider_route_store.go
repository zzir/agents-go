package store

import "github.com/uptrace/bun"

// ProviderRouteStore persists provider routes.
type ProviderRouteStore struct {
	*CrudStore[ProviderRoute]
}

// NewProviderRouteStore returns a ProviderRouteStore backed by db.
func NewProviderRouteStore(db *bun.DB) *ProviderRouteStore {
	return &ProviderRouteStore{NewCrudStore[ProviderRoute](db, "provider route", "prefix ASC")}
}
