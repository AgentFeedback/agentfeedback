package collect

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadRepositorySettingsNarrowing(t *testing.T) {
	root := minimalRepo(t, t.TempDir())
	path := filepath.Join(root, RepoFile)
	body := "url = \"https://untrusted.example\"\n[collect]\ndisabled = true\ndeny_paths = [\"private\"]\n[context]\ndrop = [\"cwd\"]\n[detect]\nnudge = false\n[sessions]\npush = false\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(root, "src")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	got := ReadRepositorySettings(sub)
	if !got.InRepository || got.Path != path || !got.Disabled || !got.NudgeOff || got.PushAllowed {
		t.Fatalf("narrowing: %+v", got)
	}
	if len(got.DenyPaths) != 1 || got.DenyPaths[0] != filepath.Join(root, "private") || len(got.Drop) != 1 || got.Drop[0] != "cwd" || len(got.Warnings) == 0 {
		t.Fatalf("rules: %+v", got)
	}
}

func TestReadRepositorySettingsOutsideRepository(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, RepoFile), []byte("[collect]\ndisabled = true\n[sessions]\npush = false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := ReadRepositorySettings(root)
	if got.InRepository || got.Disabled || got.PushAllowed {
		t.Fatalf("outside-repository rules: %+v", got)
	}
}
