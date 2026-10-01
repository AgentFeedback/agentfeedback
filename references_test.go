package agentfeedback

import (
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestReferencesCoverEveryFile catches a reference file the embed patterns
// miss, such as one in a new subdirectory of docs/ or schemas/.
func TestReferencesCoverEveryFile(t *testing.T) {
	if _, err := os.Stat("go.mod"); err != nil {
		t.Fatalf("not run from the repository root: %v", err)
	}
	var want []string
	install, err := filepath.Glob("AGENT-INSTALL*.md")
	if err != nil {
		t.Fatal(err)
	}
	want = append(want, install...)
	for _, dir := range []string{"docs", "schemas"} {
		err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			if (dir == "docs" && (strings.HasSuffix(p, ".md") || p == filepath.Join("docs", "openapi.yaml"))) ||
				(dir == "schemas" && strings.HasSuffix(p, ".json")) {
				want = append(want, filepath.ToSlash(p))
			}

			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	var got []string
	err = fs.WalkDir(References, ".", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			got = append(got, p)
		}

		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(want)
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Errorf("embedded %v\nwant %v", got, want)
	}
}
