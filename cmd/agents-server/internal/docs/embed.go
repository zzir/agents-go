// Package docs holds the swag-generated OpenAPI 3.1 spec for agents-server.
// Regenerate with `make openapi` (go tool swag init --v3.1) after changing
// any handler annotation; scripts/ci.sh fails when this directory is stale.
package docs

import _ "embed"

// SpecYAML is the raw OpenAPI 3.1 document (YAML), served at /api/v1/openapi.yaml.
//
//go:embed swagger.yaml
var SpecYAML []byte
