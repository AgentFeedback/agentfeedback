// Package agentfeedback carries the repository's reference files, embedded
// so a binary renders the agentfeedback-docs skill from exactly the docs,
// schemas, OpenAPI document and install playbooks it was built with. It
// exists because go:embed cannot reach a parent directory and the playbooks
// live at the repository root: internal/skillgen renders it into the docs
// skill; nothing else should need this package.
package agentfeedback

import "embed"

// References holds AGENT-INSTALL*.md, docs/*.md, docs/openapi.yaml and the
// JSON schemas under schemas/, at their repository paths.
//
//go:embed AGENT-INSTALL*.md docs/*.md docs/openapi.yaml schemas/*.json schemas/kinds/*.json
var References embed.FS
