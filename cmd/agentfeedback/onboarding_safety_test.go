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
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/agentfeedback/agentfeedback/v4/pkg/client"
)

func assertOnboardingSafetyOutput(t *testing.T, output string) {
	t.Helper()
	for _, secret := range []string{testKey, "old-login-key", "concurrent-login-key", "secret-device-code", "secret-verify", "secret-discovery-message", "person@example.test"} {
		if strings.Contains(output, secret) {
			t.Fatal("output disclosed private onboarding data")
		}
	}
}

func TestOnboardingLoginRefusesDestinationChangesBeforeAcquisition(t *testing.T) {
	for _, tc := range []struct {
		name      string
		body      string
		envTarget bool
		envLocal  bool
		explicit  bool
	}{
		{name: "unconfigured"},
		{name: "explicit server alone", explicit: true},
		{name: "local environment", envLocal: true, explicit: true},
		{name: "local file", body: "url = 'local'\napi_key = 'old-login-key'\n", explicit: true},
		{name: "environment differs from saved remote", body: "url = 'https://saved.example.test'\napi_key = 'old-login-key'\n", envTarget: true},
		{name: "environment differs from saved local", body: "url = 'local'\napi_key = 'old-login-key'\n", envTarget: true},
		{name: "explicit server differs from configured remote", body: "url = 'https://saved.example.test'\napi_key = 'old-login-key'\n", explicit: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			newInstallEnv(t)
			var requests atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				w.WriteHeader(http.StatusInternalServerError)
			}))
			t.Cleanup(server.Close)
			path, err := configPath(os.Getenv)
			if err != nil {
				t.Fatal(err)
			}
			if tc.body != "" {
				putFile(t, path, tc.body, 0o600)
			}
			if tc.envTarget {
				t.Setenv(envURL, server.URL)
			} else if tc.envLocal {
				t.Setenv(envURL, serverLocal)
			}
			args := []string{"login", "--email", "person@example.test", "--json"}
			if tc.explicit {
				args = append(args, "--server", server.URL)
			}
			result := runCLI(t, "", args...)
			if result.code != 2 || requests.Load() != 0 {
				t.Fatalf("destination refusal: exit %d, requests %d", result.code, requests.Load())
			}
			after, err := os.ReadFile(path)
			if tc.body == "" {
				if !os.IsNotExist(err) {
					t.Fatalf("unconfigured login created config: %v", err)
				}
			} else if err != nil || string(after) != tc.body {
				t.Fatalf("refusal changed saved config: %v", err)
			}
			assertOnboardingSafetyOutput(t, result.stdout+result.stderr)
		})
	}
}

func TestOnboardingLoginEnvironmentDestinationPreservesSettings(t *testing.T) {
	for _, savedURL := range []bool{false, true} {
		t.Run(map[bool]string{false: "environment only", true: "matching normalized saved URL"}[savedURL], func(t *testing.T) {
			newInstallEnv(t)
			t.Setenv(envMachine, "onboarding-machine")
			server := hostedOnboarding(t)
			t.Setenv(envURL, server.URL)
			path, err := configPath(os.Getenv)
			if err != nil {
				t.Fatal(err)
			}
			body := "API_KEY = 'old-login-key'\nfuture = 'retained'\n[Future]\nToken = 'retained-setting'\n[context]\ncwd = false\n[sessions]\nquiet = '9m'\n"
			if savedURL {
				body = "URL = '" + server.URL + "/'\n" + body
			}
			putFile(t, path, body, 0o600)
			before, err := configSettingsDocument(path)
			if err != nil {
				t.Fatal(err)
			}
			result := runCLI(t, "", "login", "--email", "person@example.test", "--json")
			if result.code != 0 {
				t.Fatalf("login failed with exit %d", result.code)
			}
			assertOnboardingConfig(t, server.URL, result.stdout+result.stderr)
			assertOnboardingSafetyOutput(t, result.stdout+result.stderr)
			after, err := configSettingsDocument(path)
			if err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"url", "URL", "API_KEY"} {
				delete(before, name)
				delete(after, name)
			}
			if !reflect.DeepEqual(before, after) {
				t.Fatal("login changed unrelated settings")
			}
		})
	}
}

func TestOnboardingLoginRefusesDestinationChangedDuringApproval(t *testing.T) {
	for _, savedURL := range []bool{false, true} {
		t.Run(map[bool]string{false: "destination introduced", true: "destination replaced"}[savedURL], func(t *testing.T) {
			newInstallEnv(t)
			t.Setenv(envMachine, "onboarding-machine")
			path, err := configPath(os.Getenv)
			if err != nil {
				t.Fatal(err)
			}
			changed := make(chan []byte, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/openapi.json":
					io.WriteString(w, `{"openapi":"3.1.0","paths":{"/api/v1/auth/device":{"post":{}},"/api/v1/auth/device/token":{"post":{}}}}`)
				case "/api/v1/auth/device":
					io.WriteString(w, `{"device_code":"secret-device-code","verify_url":"https://example.test/secret-verify","expires_in":20,"interval":1}`)
				case "/api/v1/auth/device/token":
					if err := updateConfigDestination(path, "https://changed.example.test", "concurrent-login-key", true); err != nil {
						t.Error(err)
						w.WriteHeader(http.StatusInternalServerError)
						return
					}
					data, err := os.ReadFile(path)
					if err != nil {
						t.Error(err)
						w.WriteHeader(http.StatusInternalServerError)
						return
					}
					changed <- data
					json.NewEncoder(w).Encode(map[string]string{"key": testKey})
				default:
					t.Error("unexpected onboarding request")
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			t.Cleanup(server.Close)
			t.Setenv(envURL, server.URL)
			body := "api_key = 'old-login-key'\nfuture = 'retained'\n[sessions]\nquiet = '9m'\n"
			if savedURL {
				body = "url = '" + server.URL + "'\n" + body
			}
			putFile(t, path, body, 0o600)
			result := runCLI(t, "", "login", "--email", "person@example.test", "--json")
			if result.code != 2 || !strings.Contains(result.stdout, "login cannot change the saved server") {
				t.Fatalf("approval race refusal: exit %d", result.code)
			}
			select {
			case expected := <-changed:
				after, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(after, expected) {
					t.Fatalf("login overwrote concurrent destination: %v", err)
				}
			default:
				t.Fatal("approval did not introduce the concurrent destination")
			}
			assertOnboardingSafetyOutput(t, result.stdout+result.stderr)
		})
	}
}

func TestOnboardingDiscoveryFailureOffersPasteOnly(t *testing.T) {
	for _, status := range []int{http.StatusServiceUnavailable, http.StatusOK} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var requests atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.URL.Path != "/api/v1/openapi.json" {
					t.Error("hosted acquisition continued after discovery failure")
				}
				w.WriteHeader(status)
				io.WriteString(w, "secret-discovery-message")
			}))
			t.Cleanup(server.Close)
			c, err := client.NewOnboardingClient(server.URL, nil)
			if err != nil {
				t.Fatal(err)
			}
			var output bytes.Buffer
			choice, err := chooseOnboarding(context.Background(), c, server.URL, bufio.NewReader(strings.NewReader("1\n")), &output)
			if err != nil || choice != (onboardingChoice{}) || requests.Load() != 1 {
				t.Fatalf("paste-only choice: %v, requests %d", err, requests.Load())
			}
			if !strings.Contains(output.String(), "Warning: could not discover authentication options") || !strings.Contains(output.String(), "authenticate with "+server.URL) || strings.Contains(output.String(), "[2]") || strings.Contains(output.String(), "[3]") || strings.Contains(output.String(), "HTTP") || strings.Contains(output.String(), "invalid onboarding response") {
				t.Fatal("discovery failure did not produce the generic destination-specific paste-only prompt")
			}
			for _, explicit := range []onboardingChoice{{email: "person@example.test"}, {agent: true}, {enrol: true}} {
				if _, err := acquireOnboarding(context.Background(), c, explicit, strings.NewReader(""), &output); err == nil {
					t.Fatal("explicit hosted acquisition ignored discovery failure")
				}
			}
			if requests.Load() != 4 {
				t.Fatalf("discovery unexpectedly retried: requests %d", requests.Load())
			}
			assertOnboardingSafetyOutput(t, output.String())
		})
	}
}

func TestOnboardingExpiryUsesConcreteRetryCommand(t *testing.T) {
	const retry = "email login expired; retry with agentfeedback login --email EMAIL --server URL"
	for _, localExpiry := range []bool{false, true} {
		t.Run(map[bool]string{false: "expired token", true: "local expiry"}[localExpiry], func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/auth/device" {
					if localExpiry {
						io.WriteString(w, `{"device_code":"secret-device-code","verify_url":"https://example.test/secret-verify","expires_in":1,"interval":2}`)
					} else {
						io.WriteString(w, `{"device_code":"secret-device-code","verify_url":"https://example.test/secret-verify","expires_in":10,"interval":1}`)
					}
					return
				}
				if localExpiry {
					t.Error("local expiry polled after the authorization deadline")
				}
				w.WriteHeader(http.StatusBadRequest)
				io.WriteString(w, `{"error":"expired_token","message":"secret-discovery-message"}`)
			}))
			t.Cleanup(server.Close)
			c, err := client.NewOnboardingClient(server.URL, nil)
			if err != nil {
				t.Fatal(err)
			}
			var output bytes.Buffer
			_, err = loginDevice(context.Background(), c, "person@example.test", "machine", &output)
			if err == nil || err.Error() != retry {
				t.Fatalf("expiry retry guidance: %v", err)
			}
			assertOnboardingSafetyOutput(t, output.String()+err.Error())
		})
	}
}
