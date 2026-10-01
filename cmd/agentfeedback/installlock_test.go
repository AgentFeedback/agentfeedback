//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package main

import (
	"fmt"
	"os"
	"strings"
	"syscall"
	"testing"
)

func TestInstall_LockHeldBeforeManifestRead(t *testing.T) {
	e := newInstallEnv(t)
	e.seed(t)
	f, err := os.Open(e.home)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, e.home)
	for _, args := range [][]string{{"install", "pi"}, {"install", "pi", "--dry-run"}, {"uninstall", "all"}} {
		r, out := installRun(t, args...)
		if r.code != 1 || !strings.Contains(fmt.Sprint(out["message"]), "another agentfeedback install or uninstall is running") {
			t.Errorf("%v: %+v", args, r)
		}
	}
	if r := runCLI(t, "", "install", "--list"); r.code != 0 {
		t.Errorf("the list waited for the lock: %+v", r)
	}
	sameTree(t, "while locked", snapshot(t, e.home), before)
}

func TestInstall_LockTakenBeforeManifestRead(t *testing.T) {
	e := newInstallEnv(t)
	e.seed(t)
	var held []bool
	afterManifestRead = func() {
		f, err := os.Open(e.home)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		held = append(held, err != nil)
	}
	t.Cleanup(func() { afterManifestRead = nil })
	for _, args := range [][]string{{"install", "pi"}, {"uninstall", "all"}} {
		if r, _ := installRun(t, args...); r.code != 0 {
			t.Fatalf("%v: %+v", args, r)
		}
	}
	if len(held) != 2 || !held[0] || !held[1] {
		t.Fatalf("lock held after the manifest read: %v", held)
	}
}
