package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestSessionsConfigDefaults(t *testing.T) {
	c := (sessionsConfig{}).config()
	if c.Quiet != 5*time.Minute || c.PolicyTTL != 5*time.Minute || c.PushBatch != 5 || c.PushBudget != 90*time.Second {
		t.Fatalf("defaults = %+v", c)
	}
}

func TestSessionsConfigParsing(t *testing.T) {
	for _, tt := range []struct {
		name   string
		body   string
		quiet  time.Duration
		ttl    time.Duration
		batch  int
		budget time.Duration
	}{
		{"explicit", "quiet = \"2m\"\npolicy_ttl = \"3m\"\npush_batch = 7\npush_budget = \"45s\"\n", 2 * time.Minute, 3 * time.Minute, 7, 45 * time.Second},
		{"nonpositive", "quiet = \"0s\"\npolicy_ttl = \"-1m\"\npush_batch = -1\npush_budget = \"0s\"\n", 5 * time.Minute, 5 * time.Minute, 5, 90 * time.Second},
	} {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			if err := os.WriteFile(path, []byte("[sessions]\n"+tt.body), 0o600); err != nil {
				t.Fatal(err)
			}
			file, exists, err := loadFileConfig(path)
			if err != nil || !exists {
				t.Fatalf("exists=%v err=%v", exists, err)
			}
			c := file.Sessions.config()
			if c.Quiet != tt.quiet || c.PolicyTTL != tt.ttl || c.PushBatch != tt.batch || c.PushBudget != tt.budget {
				t.Fatalf("config = %+v", c)
			}
		})
	}
}

func TestSessionsConfigMalformedDuration(t *testing.T) {
	for _, key := range []string{"quiet", "policy_ttl", "push_budget"} {
		for _, value := range []string{"not-duration", ""} {
			t.Run(key+"/"+value, func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "config.toml")
				if err := os.WriteFile(path, []byte("[sessions]\n"+key+" = \""+value+"\"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				_, exists, err := loadFileConfig(path)
				if !exists || err == nil || !strings.Contains(err.Error(), "sessions."+key) {
					t.Fatalf("exists=%v err=%v", exists, err)
				}
			})
		}
	}
}

func TestSessionsConfigUnknownKeys(t *testing.T) {
	data := []byte("[sessions]\nquiet = \"5m\"\npolicy_ttl = \"5m\"\npush_batch = 5\npush_budget = \"90s\"\nquuet = 1\n")
	if got := unknownConfigKeys(data); !slices.Equal(got, []string{"sessions.quuet"}) {
		t.Fatalf("unknown = %q", got)
	}
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	var problems []error
	checkConfigKeys(path, func(err error) { problems = append(problems, err) })
	if len(problems) != 1 || !strings.Contains(problems[0].Error(), "sessions.quuet") {
		t.Fatalf("problems = %v", problems)
	}
	if got := unknownConfigKeys([]byte("[sessions]\npush = true\n")); !slices.Equal(got, []string{"sessions.push"}) {
		t.Fatalf("user push switch accepted: %q", got)
	}
}
