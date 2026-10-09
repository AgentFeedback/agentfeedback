// Package agentfeedback carries the repository's reference files, embedded
// so a binary renders the agentfeedback-docs skill from exactly the docs,
// schemas, OpenAPI document and install playbooks it was built with, and the
// agentfeedback skill's install script, which the plugin bundle carries. It
// exists because go:embed cannot reach a parent directory and these files
// live at the repository root: internal/skillgen renders them into the docs
// skill and the plugin bundle; nothing else should need this package.
package agentfeedback

import "embed"

// References holds AGENT-INSTALL*.md, docs/*.md, the HTTP recipes under
// docs/recipes/, the integration docs under docs/integrations/,
// docs/openapi.yaml and the
// JSON schemas under schemas/, at their repository paths.
//
//go:embed AGENT-INSTALL*.md docs/*.md docs/recipes/*.md docs/integrations/*.md docs/openapi.yaml schemas/*.json schemas/kinds/*.json
var References embed.FS

// InstallScript is skills/agentfeedback/scripts/install.sh, which the plugin
// bundle carries beside SKILL.md.
//
//go:embed skills/agentfeedback/scripts/install.sh
var InstallScript []byte
