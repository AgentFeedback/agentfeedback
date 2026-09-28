// Package schemas carries the v1 contract's schema files, embedded so a
// binary ships exactly the files under schemas/. It exists because go:embed
// cannot reach a parent directory: pkg/schema compiles these files and is the
// package to import; nothing else should need this one.
package schemas

import "embed"

// FS holds envelope.v1.json and kinds/<kind>.v<version>.json.
//
//go:embed envelope.v1.json kinds/*.json
var FS embed.FS
