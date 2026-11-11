package sessions

import "slices"

// editTools are, per harness, the tools whose input is a file edit: a
// transcript shows their arguments (the patch, the old and new strings or
// the file content) at TranscriptEditBytes rather than TranscriptArgsBytes.
// Each name is checked against the harness's own tool definitions; a
// harness whose names could not be checked has none.
//
//   - Claude Code: Edit, MultiEdit, Write and NotebookEdit.
//   - Codex: apply_patch (openai/codex's freeform tool; its input is the
//     patch text).
//   - Gemini CLI: replace and write_file (packages/core
//     tools/definitions/base-declarations.ts).
//   - OpenCode, both readers: edit, write and patch (OpenCode 2.2.7
//     packages/core tool/plugin), and apply_patch, the V1 name OpenCode 2
//     maps onto patch.
//   - Copilot CLI: none; its session-events schema does not name tools and
//     no real log was examined.
var editTools = map[string][]string{
	HarnessClaudeCode: {"Edit", "MultiEdit", "Write", "NotebookEdit"},
	HarnessCodex:      {"apply_patch"},
	HarnessGeminiCLI:  {"replace", "write_file"},
	HarnessOpenCode:   {"edit", "write", "patch", "apply_patch"},
	HarnessOpenCodeV2: {"edit", "write", "patch", "apply_patch"},
}

// isEditTool reports whether tool is an edit tool of harness.
func isEditTool(harness, tool string) bool {
	return slices.Contains(editTools[harness], tool)
}
