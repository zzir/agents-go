package agents

import (
	"fmt"
	"maps"
	"strings"
)

// RouterProvider is a ModelProvider that routes by name prefix: "groq/llama"
// sends "llama" to the "groq" provider; an unknown prefix goes to the fallback.
type RouterProvider struct {
	routes   map[string]ModelProvider
	fallback ModelProvider
}

// NewRouterProvider builds a router from a copy of the prefix→provider map.
func NewRouterProvider(routes map[string]ModelProvider) *RouterProvider {
	cp := make(map[string]ModelProvider, len(routes))
	maps.Copy(cp, routes)
	return &RouterProvider{routes: cp}
}

// WithFallback sets the provider for names matching no route; returns the
// router for chaining.
func (r *RouterProvider) WithFallback(p ModelProvider) *RouterProvider {
	r.fallback = p
	return r
}

// Model implements ModelProvider.
func (r *RouterProvider) Model(modelName string) (Model, error) {
	if prefix, rest, found := strings.Cut(modelName, "/"); found {
		if p, ok := r.routes[prefix]; ok {
			return p.Model(rest)
		}
	}
	if r.fallback != nil {
		return r.fallback.Model(modelName)
	}
	return nil, fmt.Errorf("router: no provider for model %q (no matching prefix and no fallback set)", modelName)
}

var _ ModelProvider = (*RouterProvider)(nil)
