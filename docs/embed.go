// Package docs carries the v1 contract's OpenAPI document, embedded so a
// binary serves exactly docs/openapi.yaml. It exists because go:embed cannot
// reach a parent directory: internal/api bundles this file into the
// /api/v1/openapi.json response; nothing else should need this package.
package docs

import _ "embed"

// OpenAPI is docs/openapi.yaml verbatim.
//
//go:embed openapi.yaml
var OpenAPI []byte
