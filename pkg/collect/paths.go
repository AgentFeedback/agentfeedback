package collect

import (
	"path/filepath"
	"runtime"
	"strings"
)

// foldCase is true where the usual filesystems compare names without case.
var foldCase = runtime.GOOS == "darwin" || runtime.GOOS == "windows"

// within reports whether p is base or lies beneath it, comparing whole path
// components: /a/work contains /a/work/x but not /a/workshop. fold compares
// without case.
func within(p, base string, fold bool) bool {
	p, base = filepath.Clean(p), filepath.Clean(base)
	if fold {
		p, base = strings.ToLower(p), strings.ToLower(base)
	}
	if p == base {
		return true
	}
	if !strings.HasSuffix(base, string(filepath.Separator)) {
		base += string(filepath.Separator)
	}

	return strings.HasPrefix(p, base)
}

// forms returns the cleaned path and, when it differs, its symlink-resolved
// form.
func forms(p string) []string {
	p = filepath.Clean(p)
	out := []string{p}
	if r, err := filepath.EvalSymlinks(p); err == nil && r != p {
		out = append(out, r)
	}

	return out
}

// withinAny reports whether any form of p lies within any form of base,
// folding case where the filesystem usually does. It is the deny test: any
// way of reaching a denied directory counts.
func withinAny(p, base string) bool {
	for _, pf := range forms(p) {
		for _, bf := range forms(base) {
			if within(pf, bf, foldCase) {
				return true
			}
		}
	}

	return false
}

// resolvedWithin reports whether p, symlinks resolved (the cleaned literal
// when resolution fails), lies within any form of base, comparing case
// exactly. It is the opt-in test: a symlink into an allowed directory must
// not carry an outside directory in.
func resolvedWithin(p, base string) bool {
	p = filepath.Clean(p)
	if r, err := filepath.EvalSymlinks(p); err == nil {
		p = r
	}
	for _, bf := range forms(base) {
		if within(p, bf, false) {
			return true
		}
	}

	return false
}

// tildePath returns p relative to home as "~" or "~/rest" when p is home or
// beneath it (literally or once symlinks are resolved), else p unchanged.
func tildePath(p, home string) string {
	if home == "" || !filepath.IsAbs(home) {
		return p
	}
	for _, pf := range forms(p) {
		for _, hf := range forms(home) {
			if !within(pf, hf, foldCase) {
				continue
			}
			// within guarantees the prefix; slicing also keeps a case-folded
			// match intact where filepath.Rel would not.
			rel := strings.TrimLeft(filepath.Clean(pf)[len(filepath.Clean(hf)):], string(filepath.Separator))
			if rel == "" {
				return "~"
			}

			return "~" + string(filepath.Separator) + rel
		}
	}

	return p
}

// expandHome expands a leading "~" or "~/" with home. ok is false for "~"
// forms it cannot expand (no home, or "~user").
func expandHome(p, home string) (string, bool) {
	switch {
	case p == "~":
		return home, home != ""
	case strings.HasPrefix(p, "~/") || strings.HasPrefix(p, "~"+string(filepath.Separator)):
		return filepath.Join(home, p[2:]), home != ""
	case strings.HasPrefix(p, "~"):
		return p, false
	}

	return p, true
}
