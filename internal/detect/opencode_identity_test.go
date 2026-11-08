package detect

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestOpenCodeLegacyStateContinuity(t *testing.T) {
	t.Parallel()
	for _, limit := range []string{"interval", "session cap"} {
		t.Run(limit, func(t *testing.T) {
			cache := t.TempDir()
			const sessionID = "legacy-session"
			path := StatePath(cache, "opencode", sessionID)
			if filepath.Dir(path) != filepath.Join(cache, "sessions", "opencode") {
				t.Fatalf("legacy directory changed: %s", path)
			}
			if got := StatePath(cache, "opencode-v1", sessionID); got != path {
				t.Fatalf("canonical state path %s, want %s", got, path)
			}
			now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
			nudges, lastNudge := 1, now.Add(-time.Minute)
			if limit == "session cap" {
				nudges, lastNudge = Defaults().MaxPerSession, now.Add(-time.Hour)
			}
			seed := State{
				Tools: map[string]int{"Bash": 2}, Commands: map[string]int{CommandDigest("npm test"): 1},
				Failures: 4, Total: 12, Nudges: nudges, LastNudge: lastNudge,
				Created: now.Add(-2 * time.Hour), Updated: now.Add(-time.Minute),
			}
			data, err := json.Marshal(seed)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := Update(cache, "opencode-v1", sessionID, time.Now().Add(time.Second), func(st *State) {
				d := Decide(st, Input{
					Harness: "opencode-v1", Event: "tool.execute.after", Config: Defaults(), Now: now,
					Ev: Event{SessionID: sessionID, Tool: "Bash", Command: "npm test", ExitCode: 1, Class: ClassToolFailed},
				})
				if d.Nudge {
					t.Errorf("legacy %s was lost", limit)
				}
			}); err != nil {
				t.Fatal(err)
			}
			data, err = os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var got State
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatal(err)
			}
			if got.Tools["Bash"] != 3 || got.Commands[CommandDigest("npm test")] != 2 || got.Failures != 5 || got.Total != 13 || got.Nudges != nudges || !got.LastNudge.Equal(lastNudge) || !got.Created.Equal(seed.Created) {
				t.Fatalf("legacy state not retained: %+v", got)
			}
		})
	}
}

func TestOpenCodeCanonicalUpdateUsesLegacyLock(t *testing.T) {
	t.Parallel()
	switch runtime.GOOS {
	case "linux", "darwin", "freebsd", "netbsd", "openbsd", "dragonfly":
	default:
		t.Skip("advisory locks unavailable")
	}
	cache := t.TempDir()
	path := StatePath(cache, "opencode", "legacy-session")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	lock, err := os.OpenFile(path+lockSuffix, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Close() }()
	if !tryLock(lock) {
		t.Fatal("cannot take legacy lock")
	}
	called := false
	err = Update(cache, "opencode-v1", "legacy-session", time.Now().Add(200*time.Millisecond), func(*State) { called = true })
	if !errors.Is(err, ErrLocked) || called {
		t.Fatalf("canonical update bypassed legacy lock: err=%v, called=%v", err, called)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("state written while legacy lock held: %v", err)
	}
}
