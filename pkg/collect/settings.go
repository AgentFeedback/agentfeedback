package collect

import (
	"errors"
	"os"
	"path/filepath"
)

// RepositorySettings reports the narrowing rules read for a working directory.
// It exposes only recognised policy values, never arbitrary repository content.
type RepositorySettings struct {
	Path         string   `json:"path,omitempty"`
	Present      bool     `json:"present"`
	InRepository bool     `json:"in_repository"`
	Disabled     bool     `json:"collect_disabled"`
	DenyPaths    []string `json:"deny_paths"`
	Drop         []string `json:"context_drop"`
	NudgeOff     bool     `json:"nudge_off"`
	PushAllowed  bool     `json:"push_allowed"`
	Warnings     []string `json:"warnings,omitempty"`
}

// ReadRepositorySettings uses the same discovery and parser as collection.
// Outside Git, only the directory's session-push restriction applies.
func ReadRepositorySettings(dir string) RepositorySettings {
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	root := dir
	r, inRepo := findRepo(dir)
	if inRepo {
		root = r.root
	}
	path := filepath.Join(root, RepoFile)
	_, err := os.Lstat(path)
	rf := readRepoFile(path, root)
	out := RepositorySettings{
		Path: path, Present: !errors.Is(err, os.ErrNotExist), InRepository: inRepo,
		DenyPaths: []string{}, Drop: []string{}, Warnings: rf.warnings,
	}
	if inRepo {
		out.Disabled, out.DenyPaths, out.Drop, out.NudgeOff = rf.disabled, rf.denyPaths, rf.drop, rf.nudgeOff
		if out.DenyPaths == nil {
			out.DenyPaths = []string{}
		}
		if out.Drop == nil {
			out.Drop = []string{}
		}
	}
	out.PushAllowed = !rf.pushOff && !rf.pushInvalid
	return out
}
