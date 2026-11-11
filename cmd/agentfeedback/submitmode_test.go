package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSubmitModeLeadingDestinations(t *testing.T) {
	for _, kind := range []string{"friction", "custom", "review-stdin", "review-dir", "review-sweep"} {
		for _, remote := range []bool{false, true} {
			name := kind + "/local"
			if remote {
				name = kind + "/remote"
			}
			t.Run(name, func(t *testing.T) {
				isolateSubmit(t)
				fixClock(t, time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC))
				server := liveServer(t)
				dir := copyFixture(t)
				mode := []string{"--local"}
				if remote {
					mode = []string{"--server=" + server.srv.URL}
					t.Setenv(envURL, "http://127.0.0.1:1")
				}
				args := append([]string{"submit"}, mode...)
				input := ""
				switch kind {
				case "friction", "custom":
					args = append(args, kind, "--summary", "chosen destination")
				case "review-stdin":
					dry := runCLI(t, "", "submit", "review", dir, "--dry-run")
					if dry.code != 0 {
						t.Fatalf("fixture: %+v", dry)
					}
					input = strings.Split(dry.stdout, "\n")[0]
					args = append(args, "review", "--stdin")
				case "review-dir":
					args = append(args, "review", dir)
				case "review-sweep":
					args = append(args, "review", "--sweep", filepath.Dir(dir))
				}
				r := runCLI(t, input, args...)
				if r.code != 0 || outcomeOf(t, r)["outcome"] != "submitted" {
					t.Fatalf("submit: %+v", r)
				}
				list := runCLI(t, "", append([]string{"list", "--json"}, mode...)...)
				wantKind := kind
				if strings.HasPrefix(kind, "review-") {
					wantKind = "review"
				}
				if list.code != 0 || !strings.Contains(list.stdout, `"kind":"`+wantKind+`"`) {
					t.Fatalf("destination: %+v", list)
				}
				if !remote {
					server.mu.Lock()
					defer server.mu.Unlock()
					if len(server.log) != 0 {
						t.Fatalf("local submission reached server: %v", server.log)
					}
				} else if _, err := os.Stat(filepath.Join(dataRoot(t), "agentfeedback.db")); !os.IsNotExist(err) {
					t.Fatalf("remote submission opened local database: %v", err)
				}
			})
		}
	}
}

func TestSubmitModeAssignmentOrder(t *testing.T) {
	for _, prefix := range [][]string{{"--local=true"}, {"--local=1"}, {"--local=t"}, {"--server", "--local"}, {"--server", "--"}, {"--server", "--server"}} {
		t.Run(strings.Join(prefix, " "), func(t *testing.T) {
			isolateSubmit(t)
			server := liveServer(t)
			args := append([]string{"submit"}, prefix...)
			args = append(args, "custom", "--local=false", "--server", server.srv.URL, "--summary", "ordered")
			r := runCLI(t, "", args...)
			if r.code != 0 || outcomeOf(t, r)["outcome"] != "submitted" {
				t.Fatalf("%+v", r)
			}
			list := runCLI(t, "", "list", "--json")
			if list.code != 0 || !strings.Contains(list.stdout, `"summary":"ordered"`) {
				t.Fatalf("%+v", list)
			}
		})
	}
}

func TestSubmitModeErrorsAndHelp(t *testing.T) {
	for _, tc := range []struct {
		args []string
		code int
	}{
		{[]string{"--local"}, 2},
		{[]string{"--server"}, 2},
		{[]string{"--server=http://example.invalid"}, 2},
		{[]string{"--local=invalid", "friction"}, 2},
		{[]string{"--summary", "x", "friction"}, 2},
		{[]string{"--local", "friction", "--server", "http://example.invalid", "--summary", "x"}, 1},
		{[]string{"--server", "http://example.invalid", "friction", "--local", "--summary", "x"}, 1},
		{[]string{"--local", "--help"}, 0},
		{[]string{"--server", "http://example.invalid", "--help"}, 0},
		{[]string{"--local", "--", "friction", "--summary", "x"}, 2},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			isolateSubmit(t)
			noRequests(t)
			r := runCLI(t, "", append([]string{"submit"}, tc.args...)...)
			if r.code != tc.code {
				t.Fatalf("%+v", r)
			}
			if tc.code == 1 && outcomeOf(t, r)["reason"] != "client_setup" {
				t.Fatalf("conflicting modes: %+v", r)
			}
			if _, err := os.Stat(filepath.Join(dataRoot(t), "agentfeedback.db")); !os.IsNotExist(err) {
				t.Fatalf("created database: %v", err)
			}
		})
	}
}

func TestSubmitModeTerminatorReviewPath(t *testing.T) {
	isolateSubmit(t)
	noRequests(t)
	dir := copyFixture(t)
	r := runCLI(t, "", "submit", "--local", "--", "review", dir)
	if r.code != 0 || outcomeOf(t, r)["outcome"] != "submitted" {
		t.Fatalf("%+v", r)
	}
}

func TestSubmitModeReviewServerValueTerminator(t *testing.T) {
	for _, leading := range []bool{true, false} {
		t.Run(fmt.Sprint(leading), func(t *testing.T) {
			isolateSubmit(t)
			server := liveServer(t)
			dir := copyFixture(t)
			args := []string{"submit", "review", "--server", "--", dir}
			if leading {
				args = []string{"submit", "--server", "--", "review", dir}
			}
			args = append(args, "--server", server.srv.URL)
			r := runCLI(t, "", args...)
			if r.code != 0 || outcomeOf(t, r)["outcome"] != "submitted" {
				t.Fatalf("%+v", r)
			}
			list := runCLI(t, "", "list", "--json")
			if list.code != 0 || !strings.Contains(list.stdout, `"kind":"review"`) {
				t.Fatalf("%+v", list)
			}
		})
	}
}
