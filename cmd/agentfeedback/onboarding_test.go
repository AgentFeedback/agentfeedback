package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentfeedback/agentfeedback/v4/pkg/client"
)

func hostedOnboarding(t *testing.T) *httptest.Server {
	t.Helper()
	live := newLive(t, testKey)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/openapi.json":
			io.WriteString(w, `{"openapi":"3.1.0","paths":{"/api/v1/enrol":{"post":{}},"/api/v1/self-enrol":{"post":{}},"/api/v1/auth/device":{"post":{}},"/api/v1/auth/device/token":{"post":{}}}}`)
		case "/api/v1/self-enrol":
			json.NewEncoder(w).Encode(map[string]any{"key": testKey, "claim_url": "https://example.test/claim/secret-claim", "claim_expires_at": "2030-01-01T00:00:00Z"})
		case "/api/v1/enrol":
			var request map[string]string
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
			}
			if request["enrolment_token"] != "test-enrol-token" || request["machine_name"] != "onboarding-machine" {
				t.Errorf("unexpected enrol request fields")
			}
			json.NewEncoder(w).Encode(map[string]string{"key": testKey})
		case "/api/v1/auth/device":
			var request map[string]string
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
			}
			if request["email"] != "person@example.test" || request["machine_name"] != "onboarding-machine" {
				t.Errorf("unexpected device request fields")
			}
			io.WriteString(w, `{"device_code":"secret-device-code","verify_url":"https://example.test/verify/secret-code","expires_in":20,"interval":1}`)
		case "/api/v1/auth/device/token":
			json.NewEncoder(w).Encode(map[string]string{"key": testKey})
		default:
			live.srv.Config.Handler.ServeHTTP(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func assertOnboardingConfig(t *testing.T, server string, output string) {
	t.Helper()
	path, err := configPath(os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	cfg, _, err := loadFileConfig(path)
	if err != nil || cfg.URL != server || cfg.APIKey != testKey {
		t.Fatalf("saved credentials or server mismatch: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("configuration permissions: %v", err)
	}
	for _, secret := range []string{testKey, "test-enrol-token", "secret-device-code", "secret-code", "person@example.test"} {
		if strings.Contains(output, secret) {
			t.Errorf("terminal output disclosed %s", secret)
		}
	}
	cache, err := cacheDir(os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	err = filepath.WalkDir(cache, func(path string, entry os.DirEntry, walkErr error) error {
		if os.IsNotExist(walkErr) {
			return nil
		}
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, secret := range []string{testKey, "secret-claim", "secret-device-code", "test-enrol-token"} {
			if bytes.Contains(data, []byte(secret)) {
				t.Errorf("secret persisted in cache file %s", path)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestOnboardingInitSources(t *testing.T) {
	for _, tc := range []struct {
		name  string
		flags []string
		input string
	}{
		{"key", []string{"--key-from-stdin"}, testKey},
		{"email", []string{"--login", "person@example.test"}, ""},
		{"agent", []string{"--agent"}, ""},
		{"enrol", []string{"--enrol-token-from-stdin"}, "test-enrol-token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			newInstallEnv(t)
			t.Setenv(envMachine, "onboarding-machine")
			server := hostedOnboarding(t)
			args := append([]string{"--server", server.URL, "--harnesses", "none", "--yes"}, tc.flags...)
			result, out := initRun(t, tc.input, args...)
			if result.code != 0 || out.Status != "ok" {
				t.Fatalf("init: %+v", result)
			}
			checkE2E(t, out.E2E)
			assertOnboardingConfig(t, server.URL, result.stdout+result.stderr)
			if tc.name == "agent" && (strings.Count(result.stdout+result.stderr, "secret-claim") != 1 || out.ClaimExpiresAt != "2030-01-01T00:00:00Z" || out.ClaimNotice != claimNotice) {
				t.Fatalf("claim result: %+v", result)
			}
		})
	}
}

func TestOnboardingTerminalMenu(t *testing.T) {
	for _, choice := range []string{"1\n", "2\nperson@example.test\n", "3\n"} {
		t.Run(strings.TrimSpace(choice[:1]), func(t *testing.T) {
			newInstallEnv(t)
			t.Setenv(envMachine, "onboarding-machine")
			isTerminal = func() bool { return true }
			oldSecret := readSecret
			readSecret = func() (string, error) { return testKey, nil }
			t.Cleanup(func() { readSecret = oldSecret })
			server := hostedOnboarding(t)
			result, out := initRun(t, choice, "--server", server.URL, "--harnesses", "none")
			if result.code != 0 {
				t.Fatalf("init: %+v", result)
			}
			checkE2E(t, out.E2E)
			assertOnboardingConfig(t, server.URL, result.stdout+result.stderr)
		})
	}
}

func TestOnboardingUnsupportedAndConflicts(t *testing.T) {
	for _, flags := range [][]string{{"--login", "person@example.test"}, {"--agent"}, {"--enrol-token-from-stdin"}} {
		t.Run(flags[0], func(t *testing.T) {
			env := newInstallEnv(t)
			server := newLive(t, testKey)
			before := snapshot(t, env.home)
			result, _ := initRun(t, "", append([]string{"--server", server.srv.URL, "--yes", "--harnesses", "none"}, flags...)...)
			if result.code != 2 || !strings.Contains(result.stdout, "this server offers no") {
				t.Fatalf("unsupported: %+v", result)
			}
			sameTree(t, "unsupported auth", snapshot(t, env.home), before)
		})
	}
	t.Run("OSS menu", func(t *testing.T) {
		newInstallEnv(t)
		server := newLive(t, testKey)
		c, err := client.NewOnboardingClient(server.srv.URL, nil)
		if err != nil {
			t.Fatal(err)
		}
		var output bytes.Buffer
		choice, err := chooseOnboarding(context.Background(), c, server.srv.URL, bufio.NewReader(strings.NewReader("\n")), &output)
		if err != nil || choice != (onboardingChoice{}) || strings.Contains(output.String(), "[2]") || strings.Contains(output.String(), "[3]") {
			t.Fatalf("OSS menu: %s %v", output.String(), err)
		}
	})
	flags := [][]string{{"--key-from-stdin"}, {"--login", "person@example.test"}, {"--agent"}, {"--enrol-token-from-stdin"}}
	for i := range flags {
		for j := i + 1; j < len(flags); j++ {
			t.Run(flags[i][0]+flags[j][0], func(t *testing.T) {
				env := newInstallEnv(t)
				before := snapshot(t, env.home)
				args := append([]string{"--server", "https://example.test", "--yes"}, flags[i]...)
				args = append(args, flags[j]...)
				result, _ := initRun(t, "", args...)
				if result.code != 2 {
					t.Fatalf("conflicting sources: %+v", result)
				}
				sameTree(t, "conflicting sources", snapshot(t, env.home), before)
			})
		}
	}
}

func TestOnboardingLoginReauthentication(t *testing.T) {
	newInstallEnv(t)
	t.Setenv(envMachine, "onboarding-machine")
	server := hostedOnboarding(t)
	path, err := configPath(os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	if err := updateConfigDestination(path, server.URL, "old-key", false); err != nil {
		t.Fatal(err)
	}
	result := runCLI(t, "", "login", "--email", "person@example.test", "--json")
	if result.code != 0 {
		t.Fatalf("login: %+v", result)
	}
	assertOnboardingConfig(t, server.URL, result.stdout+result.stderr)
	result = runCLI(t, "", "login", "--json")
	if result.code != 2 {
		t.Fatalf("missing email: %+v", result)
	}
}

func TestOnboardingDeviceTransitions(t *testing.T) {
	for _, tc := range []struct {
		name      string
		responses []string
		want      string
	}{
		{"pending then key", []string{"authorization_pending", ""}, ""},
		{"expired", []string{"expired_token"}, "expired"},
		{"denied", []string{"access_denied"}, "denied"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			polls := 0
			last := time.Now()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/auth/device" {
					last = time.Now()
					io.WriteString(w, `{"device_code":"secret-device","verify_url":"https://example.test/private","expires_in":10,"interval":1}`)
					return
				}
				if time.Since(last) < time.Second {
					t.Error("polled before advertised interval")
				}
				last = time.Now()
				code := tc.responses[polls]
				polls++
				if code == "" {
					json.NewEncoder(w).Encode(map[string]string{"key": testKey})
					return
				}
				w.WriteHeader(http.StatusBadRequest)
				json.NewEncoder(w).Encode(map[string]string{"error": code, "message": "private-error"})
			}))
			defer server.Close()
			c, err := client.NewOnboardingClient(server.URL, nil)
			if err != nil {
				t.Fatal(err)
			}
			var output bytes.Buffer
			key, err := loginDevice(context.Background(), c, "person@example.test", "machine", &output)
			if tc.want == "" {
				if err != nil || key != testKey {
					t.Fatalf("login result: %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("login error: %v", err)
			}
			if polls != len(tc.responses) {
				t.Fatalf("polls: %d", polls)
			}
			for _, secret := range []string{"secret-device", "/private", "person@example.test", testKey, "private-error"} {
				if strings.Contains(output.String(), secret) {
					t.Fatal("login disclosed private data")
				}
			}
		})
	}
	t.Run("cancel and local expiry", func(t *testing.T) {
		for _, cancelled := range []bool{true, false} {
			polls := 0
			ctx, cancel := context.WithCancel(context.Background())
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/auth/device" {
					io.WriteString(w, `{"device_code":"private","verify_url":"https://example.test/private","expires_in":1,"interval":2}`)
					if cancelled {
						cancel()
					}
				} else {
					polls++
					t.Error("polled after cancellation or expiry")
				}
			}))
			c, err := client.NewOnboardingClient(server.URL, nil)
			if err != nil {
				t.Fatal(err)
			}
			_, err = loginDevice(ctx, c, "person@example.test", "machine", io.Discard)
			cancel()
			server.Close()
			if err == nil || polls != 0 {
				t.Fatalf("cancel/expiry: %v, polls %d", err, polls)
			}
		}
	})
}

func TestOnboardingCredentialsSurviveLaterRefusal(t *testing.T) {
	for _, choice := range []string{"2\nperson@example.test\nno-such-harness\n", "3\nno-such-harness\n"} {
		t.Run(choice[:1], func(t *testing.T) {
			newInstallEnv(t)
			t.Setenv(envMachine, "onboarding-machine")
			isTerminal = func() bool { return true }
			server := hostedOnboarding(t)
			result, out := initRun(t, choice, "--server", server.URL)
			if result.code != 2 || out.Status != "error" || out.Config == "" {
				t.Fatalf("expected a harness selection refusal after saving credentials: %+v", result)
			}
			assertOnboardingConfig(t, server.URL, result.stdout+result.stderr)
			if choice[0] == '3' && strings.Count(result.stdout+result.stderr, "secret-claim") != 1 {
				t.Fatal("self-enrolment claim was lost or repeated")
			}
		})
	}
}

func TestOnboardingRefusesShadowedCredentialsAndLocalTarget(t *testing.T) {
	for _, flags := range [][]string{{"--login", "person@example.test"}, {"--agent"}, {"--enrol-token-from-stdin"}} {
		for _, scenario := range []string{"environment key", "saved key", "local target"} {
			t.Run(flags[0]+"/"+scenario, func(t *testing.T) {
				env := newInstallEnv(t)
				requests := 0
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					requests++
					t.Error("refused onboarding contacted the server")
				}))
				defer server.Close()
				target := server.URL
				switch scenario {
				case "environment key":
					t.Setenv(envAPIKey, testKey)
				case "saved key":
					path, err := configPath(os.Getenv)
					if err != nil {
						t.Fatal(err)
					}
					if err := updateConfigFile(path, func(doc map[string]any) error {
						doc["api_key"] = testKey
						return nil
					}); err != nil {
						t.Fatal(err)
					}
				case "local target":
					target = "local"
				}
				before := snapshot(t, env.home)
				result, _ := initRun(t, "test-enrol-token", append([]string{"--server", target, "--yes", "--harnesses", "none"}, flags...)...)
				if result.code == 0 || requests != 0 {
					t.Fatalf("authentication should be refused before acquisition: %+v", result)
				}
				sameTree(t, "refused authentication", snapshot(t, env.home), before)
			})
		}
	}
}

func TestOnboardingClaimSurvivesConfigWriteFailure(t *testing.T) {
	newInstallEnv(t)
	path, err := configPath(os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/openapi.json" {
			io.WriteString(w, `{"openapi":"3.1.0","paths":{"/api/v1/self-enrol":{"post":{}}}}`)
			return
		}
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Error(err)
		}
		json.NewEncoder(w).Encode(map[string]string{"key": testKey, "claim_url": "https://example.test/claim/secret-claim", "claim_expires_at": "2030-01-01T00:00:00Z"})
	}))
	defer server.Close()
	result, out := initRun(t, "", "--server", server.URL, "--agent", "--harnesses", "none", "--yes")
	if result.code == 0 || out.Config != "" || strings.Count(result.stdout+result.stderr, "secret-claim") != 1 || strings.Contains(result.stdout+result.stderr, testKey) {
		t.Fatalf("write failure must retain only the claim capability in the result: %+v", result)
	}
}

func TestOnboardingRejectsUnsafeConfigBeforeConsumingCredential(t *testing.T) {
	env := newInstallEnv(t)
	path, err := configPath(os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(env.home, "missing-config"), path); err != nil {
		t.Fatal(err)
	}
	posts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posts++
		}
		io.WriteString(w, `{"openapi":"3.1.0","paths":{"/api/v1/self-enrol":{"post":{}}}}`)
	}))
	defer server.Close()
	result, _ := initRun(t, "", "--server", server.URL, "--agent", "--harnesses", "none", "--yes")
	if result.code == 0 || posts != 0 {
		t.Fatalf("unsafe config must be refused before acquiring credentials: %+v; POSTs=%d", result, posts)
	}
}
