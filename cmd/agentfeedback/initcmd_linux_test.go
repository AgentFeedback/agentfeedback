//go:build linux

package main

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"unsafe"
)

// watchOpens watches every path (a directory with everything below it, or a
// file) for opens and reads; the returned function reports each path that
// saw one since.
func watchOpens(t *testing.T, paths []string) func() []string {
	t.Helper()
	fd, err := syscall.InotifyInit1(syscall.IN_NONBLOCK | syscall.IN_CLOEXEC)
	if err != nil {
		t.Skipf("inotify: %v", err)
	}
	t.Cleanup(func() { _ = syscall.Close(fd) })
	wds := map[int32]string{}
	add := func(p string) {
		wd, err := syscall.InotifyAddWatch(fd, p, syscall.IN_OPEN|syscall.IN_ACCESS)
		if err != nil {
			t.Fatalf("watch %s: %v", p, err)
		}
		wds[int32(wd)] = p
	}
	for _, root := range paths {
		err := filepath.WalkDir(root, func(p string, _ fs.DirEntry, err error) error {
			if err == nil {
				add(p)
			}

			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	read := func() []string {
		var seen []string
		buf := make([]byte, 64<<10)
		for {
			n, err := syscall.Read(fd, buf)
			if errors.Is(err, syscall.EAGAIN) || n <= 0 {
				return seen
			}
			if err != nil {
				t.Fatal(err)
			}
			for off := 0; off+syscall.SizeofInotifyEvent <= n; {
				ev := (*syscall.InotifyEvent)(unsafe.Pointer(&buf[off]))
				name := ""
				if ev.Len > 0 {
					raw := buf[off+syscall.SizeofInotifyEvent : off+syscall.SizeofInotifyEvent+int(ev.Len)]
					for i, b := range raw {
						if b == 0 {
							raw = raw[:i]

							break
						}
					}
					name = string(raw)
				}
				seen = append(seen, filepath.Join(wds[ev.Wd], name))
				off += syscall.SizeofInotifyEvent + int(ev.Len)
			}
		}
	}
	// The walk above opened the directories it watched.
	read()

	return read
}

// TestInit_OpensNoSessionOrSecretFile runs init over a home holding session
// logs and credentials of several harnesses, and a .env in the working
// directory: none of them, and no directory of the session stores, is
// opened or read.
func TestInit_OpensNoSessionOrSecretFile(t *testing.T) {
	e := newInstallEnv(t)
	data := os.Getenv("XDG_DATA_HOME")
	work := t.TempDir()
	t.Chdir(work)
	sessions := map[string]string{
		filepath.Join(e.home, ".claude", "projects"):                              "-work/0b6a5c3e.jsonl",
		filepath.Join(e.home, ".codex", "sessions"):                               "2026/10/08/rollout-2026-10-08T10-00-00-0b6a5c3e.jsonl",
		filepath.Join(e.home, ".copilot", "session-state"):                        "0b6a5c3e/events.jsonl",
		filepath.Join(e.home, ".gemini", "tmp"):                                   "work/chats/session-0b6a5c3e.jsonl",
		filepath.Join(data, "opencode"):                                           "opencode.db",
		filepath.Join(e.home, ".gemini", "antigravity", "brain"):                  "0b6a5c3e/.system_generated/logs/transcript.jsonl",
		filepath.Join(e.home, ".cursor", "projects", "work", "agent-transcripts"): "0b6a5c3e.jsonl",
	}
	var watched []string
	for root, file := range sessions {
		putFile(t, filepath.Join(root, file), "{\"type\":\"user\"}\n", 0o600)
		watched = append(watched, root)
	}
	secrets := []string{
		filepath.Join(e.home, ".claude", ".credentials.json"),
		filepath.Join(e.home, ".codex", "auth.json"),
		filepath.Join(e.home, ".gemini", "oauth_creds.json"),
		filepath.Join(e.home, ".config", "gh", "hosts.yml"),
		filepath.Join(e.home, ".ssh", "id_ed25519"),
		filepath.Join(work, ".env"),
	}
	for _, s := range secrets {
		putFile(t, s, "token = \"throwaway\"\n", 0o600)
	}
	// The harnesses above count as detected by their directories.
	e.detectHarnesses(t, ".config/opencode")
	seen := watchOpens(t, append(watched, secrets...))

	r, out := initRun(t, "", "--yes")
	if r.code != 0 || out.Status != "ok" || len(out.Harnesses) < 5 {
		t.Fatalf("init: %+v", r)
	}
	checkE2E(t, out.E2E)
	if s := seen(); len(s) > 0 {
		t.Fatalf("init opened or read %q", s)
	}
	// The watch itself sees a read.
	if _, err := os.ReadFile(secrets[0]); err != nil {
		t.Fatal(err)
	}
	if s := seen(); len(s) == 0 {
		t.Fatal("the watch saw no open of a file it watches")
	}
}
