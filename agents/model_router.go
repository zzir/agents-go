package agents

import (
	"fmt"
	"maps"
	"strings"
)

// RouterProvider routes model names to different ModelProviders by a name
// prefix, so a single run can drive each agent against a different backend by
// model name alone. It implements ModelProvider.
//
// A name like "groq/llama-3.3-70b" is split on the first separator ("/" by
// default): the prefix ("groq") selects the provider and the remainder
// ("llama-3.3-70b") is the model name passed to it. Names without a known prefix
// go to the fallback provider, if one is set.
type RouterProvider struct {
	routes    map[string]ModelProvider
	fallback  ModelProvider
	separator string
}

// NewRouterProvider builds a router from a prefix→provider map. The map is
// copied, so later mutations to the caller's map do not affect the router.
func NewRouterProvider(routes map[string]ModelProvider) *RouterProvider {
	cp := make(map[string]ModelProvider, len(routes))
	maps.Copy(cp, routes)
	return &RouterProvider{routes: cp, separator: "/"}
}

// WithFallback sets the provider used for model names whose prefix matches no
// route (including names with no separator). Returns the router for chaining.
func (r *RouterProvider) WithFallback(p ModelProvider) *RouterProvider {
	r.fallback = p
	return r
}

// WithSeparator overrides the prefix separator (default "/"). Returns the router
// for chaining.
func (r *RouterProvider) WithSeparator(sep string) *RouterProvider {
	if sep != "" {
		r.separator = sep
	}
	return r
}

// GetModel implements ModelProvider.
func (r *RouterProvider) GetModel(modelName string) (Model, error) {
	if prefix, rest, found := strings.Cut(modelName, r.separator); found {
		if p, ok := r.routes[prefix]; ok {
			return p.GetModel(rest)
		}
	}
	if r.fallback != nil {
		return r.fallback.GetModel(modelName)
	}
	return nil, fmt.Errorf("router: no provider for model %q (no matching prefix and no fallback set)", modelName)
}

var _ ModelProvider = (*RouterProvider)(nil)
