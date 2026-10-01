package codexconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPrepareAndApplyPreserveUnrelatedTOMLAndFirstBackup(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "config.toml")
	original := []byte(`# a root comment
"openai_base_url" = "https://old.invalid/v1" # retain this comment
multiline = """
an unrelated value with model_catalog_json = "not a key"
"""
editor.theme = "dark"

[plugins.example]
enabled = true
`)
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}

	plan, err := Prepare(path, "http://127.0.0.1:4317/v1", "/catalog-one.json")
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Changed || !plan.OriginalExists {
		t.Fatalf("plan = %+v", plan)
	}
	if err := plan.Apply(); err != nil {
		t.Fatal(err)
	}
	backup, err := os.ReadFile(plan.Backup)
	if err != nil {
		t.Fatal(err)
	}
	if string(backup) != string(original) {
		t.Fatalf("backup changed original bytes:\n%s", backup)
	}
	updated, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"openai_base_url" = "http://127.0.0.1:4317/v1" # retain this comment`,
		`model_catalog_json = "/catalog-one.json"`,
		`an unrelated value with model_catalog_json = "not a key"`,
		`[plugins.example]`,
	} {
		if !strings.Contains(string(updated), want) {
			t.Fatalf("updated configuration lost %q:\n%s", want, updated)
		}
	}

	second, err := Prepare(path, "http://127.0.0.1:4317/v1", "/catalog-two.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Apply(); err != nil {
		t.Fatal(err)
	}
	backup, err = os.ReadFile(plan.Backup)
	if err != nil {
		t.Fatal(err)
	}
	if string(backup) != string(original) {
		t.Fatal("a later update replaced the first exact backup")
	}
	updated, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(updated), `model_catalog_json = "/catalog-two.json"`) {
		t.Fatalf("later catalog change was not applied:\n%s", updated)
	}
}

func TestPrepareRejectsProviderAndLeavesInputUntouched(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	original := []byte("model_provider = \"custom\"\n[plugins]\nenabled = true\n")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Prepare(path, "http://127.0.0.1:4317/v1", "/catalog.json"); err == nil || !strings.Contains(err.Error(), "model_provider") {
		t.Fatalf("error = %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(original) {
		t.Fatal("provider error changed Codex configuration")
	}
}

func TestPrepareBacksUpAnExistingEmptyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	plan, err := Prepare(path, "http://127.0.0.1:4317/v1", "/catalog.json")
	if err != nil {
		t.Fatal(err)
	}
	if !plan.OriginalExists {
		t.Fatal("an existing empty file must be backed up")
	}
	if err := plan.Apply(); err != nil {
		t.Fatal(err)
	}
	backup, err := os.ReadFile(plan.Backup)
	if err != nil {
		t.Fatal(err)
	}
	if len(backup) != 0 {
		t.Fatalf("empty backup = %q", backup)
	}
}

func TestApplyRefusesANonRegularExistingBackup(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "config.toml")
	original := []byte("[plugins]\nenabled = true\n")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	plan, err := Prepare(path, "http://127.0.0.1:4317/v1", "/catalog.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(plan.Backup, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := plan.Apply(); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("Apply error = %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(original) {
		t.Fatal("a bad backup path changed the Codex configuration")
	}
}

func TestApplyRefusesConfigurationChangedAfterPrepare(t *testing.T) {
	baseURL, catalogPath := "http://127.0.0.1:4317/v1", "/catalog.json"
	for _, test := range []struct {
		name          string
		original      []byte
		concurrent    []byte
		deleteCurrent bool
	}{
		{name: "existing edited", original: []byte("[plugins]\nenabled = true\n"), concurrent: []byte("[plugins]\nenabled = false\n")},
		{name: "existing deleted", original: []byte("[plugins]\nenabled = true\n"), deleteCurrent: true},
		{name: "absent created", concurrent: []byte("[plugins]\nenabled = true\n")},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			if test.original != nil {
				if err := os.WriteFile(path, test.original, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			plan, err := Prepare(path, baseURL, catalogPath)
			if err != nil {
				t.Fatal(err)
			}
			if test.deleteCurrent {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.WriteFile(path, test.concurrent, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := plan.Apply(); err == nil || !strings.Contains(err.Error(), "changed since it was prepared") {
				t.Fatalf("Apply error = %v", err)
			}
			if test.deleteCurrent {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatalf("deleted configuration was recreated: %v", err)
				}
			} else {
				after, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if string(after) != string(test.concurrent) {
					t.Fatalf("concurrent edit was overwritten: %q", after)
				}
			}
			if _, err := os.Stat(plan.Backup); !os.IsNotExist(err) {
				t.Fatalf("concurrent change created a backup: %v", err)
			}
		})
	}
}

func TestDefaultPathUsesOnlyAbsoluteCodexHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	absolute := t.TempDir()
	t.Setenv("CODEX_HOME", absolute)
	path, err := DefaultPath()
	if err != nil {
		t.Fatal(err)
	}
	if path != filepath.Join(absolute, "config.toml") {
		t.Fatalf("path = %q", path)
	}
	t.Setenv("CODEX_HOME", "relative-codex-home")
	path, err = DefaultPath()
	if err != nil {
		t.Fatal(err)
	}
	if path != filepath.Join(home, ".codex", "config.toml") {
		t.Fatalf("relative CODEX_HOME path = %q", path)
	}
}
