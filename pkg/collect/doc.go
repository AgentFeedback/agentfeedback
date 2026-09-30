// Package collect gathers the context a client submission carries and decides
// whether a submission may be collected at all.
//
// Collect fills the project group (repository facts read from .git metadata,
// with git when it is on PATH and by parsing the files when it is not; both
// paths give the same values except git_dirty, which needs git), the machine
// group and the harness group (an allow-list of environment variables, looked
// up by name through an injected Getenv), plus the non-code keys the caller
// passes in. It is best-effort: a source that fails only omits its keys.
//
// Check applies the narrowing rules: the user config's [collect] table and a
// repository's .agentfeedback.toml, which may only narrow.
//
// Nothing here opens a network connection, enumerates processes, reads
// anything under /proc or /sys, or reads a file other than git metadata and
// the repository's .agentfeedback.toml; every read is bounded and refuses
// anything but a regular file. Git runs without the process's GIT_*
// variables, and git status (git_dirty) only when the repository config
// names no filter driver and includes no other file.
//
// The home directory's path, which carries the username, is not collected
// by default: repo_root and a local git_remote under it are "~"-relative,
// and folder is "~" for the home directory itself. Only context.cwd, which
// is opt-in, sends the full working directory.
//
// Known limitation without git: an annotated tag whose ref is loose and
// whose tag object is only in a pack is not reported as git_tag.
package collect
