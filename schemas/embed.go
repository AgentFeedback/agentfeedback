// Package schemas carries the v1 contract's schema files, embedded so a
// binary ships exactly the files under schemas/. It exists because go:embed
// cannot reach a parent directory: pkg/schema compiles the envelope and kind
// files and is the package to import for them; internal/report compiles
// checkup-report.v1.json, which is not a submission schema.
package schemas

import "embed"

// FS holds envelope.v1.json, kinds/<kind>.v<version>.json and
// checkup-report.v1.json.
//
//go:embed envelope.v1.json kinds/*.json checkup-report.v1.json
var FS embed.FS
