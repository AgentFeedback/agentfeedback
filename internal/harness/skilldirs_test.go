package harness

import (
	"path/filepath"
	"strings"
	"testing"
)

// Install's whole-file skill items (the skill, the docs and the triage
// skills) assume no two harnesses share a skills directory: two records
// would then claim the same file.
func TestSkillDirsDisjoint(t *testing.T) {
	e := testEnv(t)
	type dir struct{ harness, path string }
	var dirs []dir
	for _, a := range registry {
		for _, d := range a.SkillDirs(e) {
			dirs = append(dirs, dir{a.Name, filepath.Clean(d)})
		}
	}
	sep := string(filepath.Separator)
	for i, a := range dirs {
		for j, b := range dirs {
			if i == j {
				continue
			}
			if a.path == b.path || strings.HasPrefix(b.path, a.path+sep) {
				t.Errorf("%s's skills directory %s overlaps %s's %s", a.harness, a.path, b.harness, b.path)
			}
		}
	}
}
