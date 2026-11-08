package main

import (
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestConfigDestinationPreservesSettings(t *testing.T) {
	for _, force := range []bool{false, true} {
		t.Run(map[bool]string{false: "settings-only", true: "replace-destination"}[force], func(t *testing.T) {
			body := "future = 'retained'\n[collect]\ndisabled = true\n[context]\ncwd = false\n[sessions]\nquiet = '9m'\n"
			if force {
				body = "URL = 'https://old.example'\nAPI_KEY = 'old-test-key'\n" + body
			}
			getenv, path := configSettingsTestEnv(t, body)
			before, err := configSettingsDocument(path)
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := runDoctorInit(getenv, "https://new.example", true, force, strings.NewReader("new-test-key")); err != nil {
				t.Fatal(err)
			}
			after, err := configSettingsDocument(path)
			if err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"url", "URL", "api_key", "API_KEY"} {
				delete(before, name)
				delete(after, name)
			}
			if !reflect.DeepEqual(before, after) {
				t.Fatalf("settings changed: before=%v after=%v", before, after)
			}
			file, _, err := loadFileConfig(path)
			if err != nil || file.URL != "https://new.example" || file.APIKey != "new-test-key" {
				t.Fatalf("destination update failed: %v", err)
			}
		})
	}
}

func TestConfigDestinationRefusesAmbiguousOrSymlinkedFile(t *testing.T) {
	for _, body := range []string{
		"url = 'https://one.example'\nURL = 'https://two.example'\n",
		"[collect]\ndisabled = true\nDISABLED = false\n",
	} {
		_, path := configSettingsTestEnv(t, body)
		if err := updateConfigDestination(path, "https://new.example", "test-key", true); err == nil {
			t.Fatal("accepted ambiguous config")
		}
		data, err := os.ReadFile(path)
		if err != nil || string(data) != body {
			t.Fatalf("refusal modified config: %v", err)
		}
	}
	getenv, path := configSettingsTestEnv(t, "[collect]\ndisabled = true\n")
	target := path + ".target"
	if err := os.Rename(path, target); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := setConfigSetting(getenv, "collect.disabled", "false"); err == nil {
		t.Fatal("accepted symlinked config")
	}
	if err := updateConfigDestination(path, "https://new.example", "test-key", true); err == nil {
		t.Fatal("replaced symlinked config")
	}
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("config symlink replaced: %v", err)
	}
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "[collect]\ndisabled = true\n" {
		t.Fatalf("symlink target modified: %v", err)
	}
}
