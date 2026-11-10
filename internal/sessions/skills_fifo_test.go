//go:build unix

package sessions

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// TestSkillsFIFO: a SKILL.md that is a FIFO is a problem, never opened (an
// open would block until a writer comes).
func TestSkillsFIFO(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".claude", "skills", "pipe")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(dir, "SKILL.md"), 0o644); err != nil {
		t.Skip("mkfifo:", err)
	}
	done := make(chan Inventory, 1)
	go func() {
		inv, _ := Skills(Env{Home: home, SkillDirs: skillDirsOf(home)}, SkillsRequest{Global: true})
		done <- inv
	}()
	select {
	case inv := <-done:
		if len(inv.Skills) != 0 || len(inv.Problems) != 1 || inv.Problems[0].Reason != reasonNotRegular {
			t.Fatalf("inventory: %+v", inv)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Skills blocked on a FIFO")
	}
}
