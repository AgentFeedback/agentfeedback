package core

import (
	"os/exec"
	"strings"
	"testing"
)

// TestNoNetHTTP keeps the core transport-neutral: net/http must not be in
// its dependency closure.
func TestNoNetHTTP(t *testing.T) {
	t.Parallel()
	goTool, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go not on PATH")
	}
	out, err := exec.Command(goTool, "list", "-deps", ".").Output()
	if err != nil {
		t.Fatalf("go list -deps: %v", err)
	}
	for _, dep := range strings.Fields(string(out)) {
		if dep == "net/http" {
			t.Fatal("internal/core depends on net/http")
		}
	}
}
