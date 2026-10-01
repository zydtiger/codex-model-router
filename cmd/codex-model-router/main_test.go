package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"io"

	"github.com/zydtiger/codex-model-router/internal/catalog"
	"github.com/zydtiger/codex-model-router/internal/config"
	"github.com/zydtiger/codex-model-router/internal/routing"
	"github.com/zydtiger/codex-model-router/internal/serve"
	"github.com/zydtiger/codex-model-router/internal/service"
)

// workspace is a temporary set of files the CLI can act on. Both native URLs point
// at a mock server so no subcommand can reach a real account.
type workspace struct {
	dir           string
	configPath    string
	catalog       string
	nativeCatalog string
	upstream      string
	calls         *callLog
}

type callLog struct {
	mu    sync.Mutex
	paths []string
}

type fakeSetupService struct {
	installs int
	previews int
	options  service.Options
}

func (s *fakeSetupService) Preview() (string, service.Resolved, error) {
	s.previews++
	return "", service.Resolved{}, nil
}

func (s *fakeSetupService) Install(_ context.Context, _, _ bool) (service.Report, error) {
	s.installs++
	return service.Report{}, nil
}

func (c *callLog) record(path string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.paths = append(c.paths, path)
}

func (c *callLog) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.paths)
}

func newWorkspace(t *testing.T) *workspace {
	t.Helper()
	calls := &callLog{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.record(r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_1","object":"response","status":"completed","output":[]}`))
	}))
	t.Cleanup(upstream.Close)

	dir := t.TempDir()
	nativeCatalog := filepath.Join(dir, "native-catalog.json")
	if err := os.WriteFile(nativeCatalog, []byte(`{
	  "models": [{
	    "slug": "gpt-native", "display_name": "GPT Native", "description": "Native",
	    "default_reasoning_level": "medium",
	    "supported_reasoning_levels": [{"effort":"medium","description":"Medium reasoning"}],
	    "shell_type": "unified_exec", "visibility": "list", "supported_in_api": true, "priority": 0,
	    "context_window": 272000, "max_context_window": 272000, "supports_parallel_tool_calls": true,
	    "input_modalities": ["text"], "supports_search_tool": true, "supports_image_detail_original": true,
	    "truncation_policy": {"mode":"tokens","limit":10000}, "supports_verbosity": true,
	    "non_reasoning": false, "experimental_supported_tools": [], "model_messages": null,
	    "base_instructions": "Synthetic instructions."
	  }]
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	instructions := filepath.Join(dir, "instructions.txt")
	if err := os.WriteFile(instructions, []byte("You are a coding agent on a lab server.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, "config.json")
	body := `{
  "listen": {"host": "127.0.0.1", "port": 4317},
  "log": {"level": "warn"},
  "native": {
    "chatgpt_base_url": "` + upstream.URL + `/backend-api/codex",
    "api_base_url": "` + upstream.URL + `/v1",
    "preserve_client_auth": true,
    "models": ["gpt-native"]
  },
  "catalog": {
    "native_catalog_file": "` + filepath.ToSlash(nativeCatalog) + `",
    "output_file": "catalog.json",
    "base_instructions_file": "` + filepath.ToSlash(instructions) + `",
    "models": [{
      "id": "qwen3-32b",
      "route": "lab-qwen3-32b",
      "display_name": "Qwen3 32B (lab)",
      "context_window": 131072,
      "default_reasoning_level": "medium",
      "tool_capable": true
    }]
  },
  "routes": [{
    "name": "lab-qwen3-32b",
    "base_url": "` + upstream.URL + `/v1",
    "models": ["qwen3-32b"],
    "reasoning": {
      "adapter": "reasoning_to_chat_template",
      "supported_efforts": ["none", "low", "medium", "high"],
      "chat_template_kwargs": {
        "enable_thinking": {"none": false, "low": true, "medium": true, "high": true},
        "reasoning_effort": {"none": null, "low": "low", "medium": "medium", "high": "high"},
        "preserve_thinking": true
      }
    },
    "tools": {
      "namespace_adapter": "namespace_to_functions",
      "custom_adapter": "custom_to_functions",
      "schema_loading": "on_demand"
    }
  }]
}`
	if err := os.WriteFile(configPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return &workspace{
		dir:           dir,
		configPath:    configPath,
		catalog:       filepath.Join(dir, "catalog.json"),
		nativeCatalog: nativeCatalog,
		upstream:      upstream.URL,
		calls:         calls,
	}
}

func configureRouteCredential(t *testing.T, work *workspace, name string) {
	t.Helper()
	data, err := os.ReadFile(work.configPath)
	if err != nil {
		t.Fatal(err)
	}
	updated := strings.Replace(string(data),
		`"models": ["qwen3-32b"],
    "reasoning":`,
		`"models": ["qwen3-32b"],
    "auth": {"api_key_env": "`+name+`"},
    "reasoning":`, 1)
	if updated == string(data) {
		t.Fatal("test fixture does not contain the route model list")
	}
	if err := os.WriteFile(work.configPath, []byte(updated), 0o600); err != nil {
		t.Fatal(err)
	}
}

func configureNativeFallback(t *testing.T, work *workspace, name string) {
	t.Helper()
	data, err := os.ReadFile(work.configPath)
	if err != nil {
		t.Fatal(err)
	}
	updated := strings.Replace(string(data),
		`"preserve_client_auth": true,`,
		`"preserve_client_auth": true,
    "api_key_env": "`+name+`",`, 1)
	if updated == string(data) {
		t.Fatal("test fixture does not contain preserve_client_auth")
	}
	if err := os.WriteFile(work.configPath, []byte(updated), 0o600); err != nil {
		t.Fatal(err)
	}
}

func configureNativeCredentialRequirement(t *testing.T, work *workspace, name string) {
	t.Helper()
	data, err := os.ReadFile(work.configPath)
	if err != nil {
		t.Fatal(err)
	}
	updated := strings.Replace(string(data),
		`"preserve_client_auth": true,`,
		`"preserve_client_auth": false,
    "api_key_env": "`+name+`",`, 1)
	if updated == string(data) {
		t.Fatal("test fixture does not contain preserve_client_auth")
	}
	if err := os.WriteFile(work.configPath, []byte(updated), 0o600); err != nil {
		t.Fatal(err)
	}
}

func configureDefaultCatalogPath(t *testing.T, work *workspace) {
	t.Helper()
	data, err := os.ReadFile(work.configPath)
	if err != nil {
		t.Fatal(err)
	}
	updated := strings.Replace(string(data), `"output_file": "catalog.json"`, `"output_file": ""`, 1)
	if updated == string(data) {
		t.Fatal("test fixture does not contain catalog output_file")
	}
	if err := os.WriteFile(work.configPath, []byte(updated), 0o600); err != nil {
		t.Fatal(err)
	}
}

// runCLI runs one subcommand and returns its exit code plus captured output.
func runCLI(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func mustRun(t *testing.T, want int, args ...string) string {
	t.Helper()
	code, stdout, stderr := runCLI(t, args...)
	if code != want {
		t.Fatalf("%v exited %d, want %d\nstdout:\n%s\nstderr:\n%s", args, code, want, stdout, stderr)
	}
	return stdout
}

func TestVersionAndUsage(t *testing.T) {
	out := mustRun(t, exitOK, "version")
	if !strings.Contains(out, version) {
		t.Fatalf("version output = %q", out)
	}
	if code, _, _ := runCLI(t, "help"); code != exitOK {
		t.Fatalf("help exited %d", code)
	}
	for _, args := range [][]string{{}, {"not-a-command"}, {"catalog"}, {"codex-config"}, {"service"}} {
		if code, _, _ := runCLI(t, args...); code != exitUsage {
			t.Fatalf("%v exited %d, want %d", args, code, exitUsage)
		}
	}
	for _, args := range [][]string{{"catalog", "generate", "-h"}, {"setup", "-h"}} {
		if code, _, _ := runCLI(t, args...); code != exitOK {
			t.Fatalf("%v exited %d, want help success", args, code)
		}
	}
}

func TestUsageListsEverySubcommand(t *testing.T) {
	out := mustRun(t, exitOK, "help")
	for _, name := range []string{"serve", "validate", "catalog generate", "catalog print-example",
		"setup", "service preview", "service install", "service status", "service uninstall", "healthcheck", "version"} {
		if !strings.Contains(out, name) {
			t.Fatalf("usage does not mention %q:\n%s", name, out)
		}
	}
}

func TestSetupUsesCurrentExecutableUnlessBinIsExplicit(t *testing.T) {
	current, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := setupBinary("")
	if err != nil {
		t.Fatal(err)
	}
	if resolved != current {
		t.Fatalf("default setup binary = %q, want current executable %q", resolved, current)
	}
	explicit, err := setupBinary("relative-router")
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(explicit) || filepath.Base(explicit) != "relative-router" {
		t.Fatalf("explicit setup binary = %q", explicit)
	}
}

func TestValidate(t *testing.T) {
	work := newWorkspace(t)
	out := mustRun(t, exitOK, "validate", "--config", work.configPath)
	for _, want := range []string{"configuration is valid", "qwen3-32b", "gpt-native", "/healthz"} {
		if !strings.Contains(out, want) {
			t.Fatalf("validate output is missing %q:\n%s", want, out)
		}
	}
	// The reasoning and tools adapters are reported independently.
	for _, want := range []string{
		"reasoning:", "reasoning_to_chat_template",
		"tools:", "namespace_to_functions", "schema_loading=on_demand",
		"custom_to_functions",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("validate output is missing %q:\n%s", want, out)
		}
	}

	jsonOut := mustRun(t, exitOK, "validate", "--config", work.configPath, "--json")
	var summary struct {
		OK     bool   `json:"ok"`
		Listen string `json:"listen"`
		Routes []struct {
			Name      string `json:"name"`
			Reasoning string `json:"reasoning"`
			Tools     struct {
				NamespaceAdapter string `json:"namespace_adapter"`
				CustomAdapter    string `json:"custom_adapter"`
				SchemaLoading    string `json:"schema_loading"`
			} `json:"tools"`
		} `json:"routes"`
	}
	if err := json.Unmarshal([]byte(jsonOut), &summary); err != nil {
		t.Fatalf("--json output is not JSON: %v\n%s", err, jsonOut)
	}
	if !summary.OK || summary.Listen != "127.0.0.1:4317" || len(summary.Routes) != 1 {
		t.Fatalf("summary = %s", jsonOut)
	}
	route := summary.Routes[0]
	if route.Name != "lab-qwen3-32b" ||
		route.Reasoning != "reasoning_to_chat_template" ||
		route.Tools.NamespaceAdapter != "namespace_to_functions" ||
		route.Tools.CustomAdapter != "custom_to_functions" ||
		route.Tools.SchemaLoading != "on_demand" {
		t.Fatalf("route summary lost the independent adapters: %s", jsonOut)
	}

	broken := filepath.Join(work.dir, "broken.json")
	if err := os.WriteFile(broken, []byte(`{"listen":{"host":"0.0.0.0","port":4317}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := runCLI(t, "validate", "--config", broken)
	if code != exitConfig {
		t.Fatalf("an invalid config exited %d, want %d: %s", code, exitConfig, stderr)
	}
	if !strings.Contains(stderr, "listen.host") {
		t.Fatalf("the reason was not reported: %s", stderr)
	}

	if code, _, _ := runCLI(t, "validate", "--config", filepath.Join(work.dir, "absent.json")); code != exitConfig {
		t.Fatalf("a missing config exited %d, want %d", code, exitConfig)
	}
	if code, _, _ := runCLI(t, "validate", "--config", work.configPath, "extra"); code != exitUsage {
		t.Fatalf("a positional argument exited %d, want %d", code, exitUsage)
	}
}

func TestCatalogPrintExampleIsValidConfiguration(t *testing.T) {
	work := newWorkspace(t)
	output := filepath.Join(work.dir, "example.json")
	mustRun(t, exitOK, "catalog", "print-example", "--out", output)
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	// The example ships the documented public native endpoint on purpose; what must
	// never appear is somebody's own machine, path, or key.
	for _, forbidden := range []string{"/Users/", "/home/", "sk-", "C:\\", "@gmail.com"} {
		if strings.Contains(string(data), forbidden) {
			t.Fatalf("the example configuration contains %q", forbidden)
		}
	}
	// The example must be usable as-is.
	mustRun(t, exitOK, "validate", "--config", output)
}

func TestCatalogGenerate(t *testing.T) {
	work := newWorkspace(t)
	output := filepath.Join(work.dir, "generated.json")
	out := mustRun(t, exitOK, "catalog", "generate", "--config", work.configPath, "--out", output)
	if !strings.Contains(out, "2 model entries") {
		t.Fatalf("output = %s", out)
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Models []struct {
			Slug             string `json:"slug"`
			DisplayName      string `json:"display_name"`
			ContextWindow    int    `json:"context_window"`
			BaseInstructions string `json:"base_instructions"`
		} `json:"models"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("the generated catalog is not valid JSON: %v", err)
	}
	if len(decoded.Models) != 2 {
		t.Fatalf("entries = %d, want 2: %s", len(decoded.Models), data)
	}
	var native, local bool
	for _, entry := range decoded.Models {
		switch entry.Slug {
		case "gpt-native":
			native = true
		case "qwen3-32b":
			local = true
			if entry.DisplayName != "Qwen3 32B (lab)" || entry.ContextWindow != 131072 {
				t.Fatalf("the local entry lost its configured fields: %+v", entry)
			}
			if !strings.Contains(entry.BaseInstructions, "lab server") {
				t.Fatalf("base_instructions was not read from the file: %q", entry.BaseInstructions)
			}
		}
	}
	if !native || !local {
		t.Fatalf("native=%v local=%v", native, local)
	}

	// Generating into the configured output_file with no --out also works.
	mustRun(t, exitOK, "catalog", "generate", "--config", work.configPath)
	if _, err := os.Stat(work.catalog); err != nil {
		t.Fatalf("the configured output_file was not written: %v", err)
	}
	// The parent directory of an explicit --out is created, so a fresh checkout can
	// generate straight into a new location.
	nested := filepath.Join(work.dir, "generated", "nested", "catalog.json")
	mustRun(t, exitOK, "catalog", "generate", "--config", work.configPath, "--out", nested)
	if _, err := os.Stat(nested); err != nil {
		t.Fatalf("the nested output was not written: %v", err)
	}
	// A directory is not a file target.
	if code, _, _ := runCLI(t, "catalog", "generate", "--config", work.configPath, "--out", work.dir); code != exitError {
		t.Fatalf("using a directory as --out exited %d, want %d", code, exitError)
	}
	if code, _, _ := runCLI(t, "catalog", "bogus"); code != exitUsage {
		t.Fatalf("an unknown catalog subcommand exited %d", code)
	}

	// A failed raw input must leave an already usable final catalog untouched.
	before, err := os.ReadFile(work.catalog)
	if err != nil {
		t.Fatal(err)
	}
	broken := filepath.Join(work.dir, "broken-native.json")
	if err := os.WriteFile(broken, []byte("not JSON"), 0o600); err != nil {
		t.Fatal(err)
	}
	configData, err := os.ReadFile(work.configPath)
	if err != nil {
		t.Fatal(err)
	}
	brokenConfig := filepath.Join(work.dir, "broken-config.json")
	if err := os.WriteFile(brokenConfig, []byte(strings.Replace(string(configData), filepath.ToSlash(work.nativeCatalog), filepath.ToSlash(broken), 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := runCLI(t, "catalog", "generate", "--config", brokenConfig); code != exitError {
		t.Fatalf("invalid native JSON exited %d, want %d", code, exitError)
	}
	after, err := os.ReadFile(work.catalog)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("catalog generation failure replaced the old catalog")
	}
}

func TestCatalogGenerateExportsNativeModelsWhenNoRawFileIsConfigured(t *testing.T) {
	directory := t.TempDir()
	configPath := filepath.Join(directory, "config.json")
	output := filepath.Join(directory, "catalog.json")
	configText := `{
  "listen": {"host": "127.0.0.1", "port": 4317},
  "catalog": {"output_file": "` + filepath.ToSlash(output) + `"},
  "routes": [],
  "native": {"models": []}
}`
	if err := os.WriteFile(configPath, []byte(configText), 0o600); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(directory, "codex-home")
	exporter := filepath.Join(directory, "fake-codex")
	script := "#!/bin/sh\nprintf '%s' \"$CODEX_HOME\" > " + strconv.Quote(marker) + "\nprintf '%s\\n' '{\"models\":[{\"slug\":\"gpt-native\"}]}'\n"
	if err := os.WriteFile(exporter, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", filepath.Join(directory, "user-codex-home"))
	mustRun(t, exitOK, "catalog", "generate", "--config", configPath, "--codex", exporter)
	home, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	if string(home) == os.Getenv("CODEX_HOME") || !filepath.IsAbs(string(home)) {
		t.Fatalf("export used the user Codex home: %q", home)
	}
	if _, err := os.Stat(string(home)); !os.IsNotExist(err) {
		t.Fatalf("temporary Codex home remains after export: %v", err)
	}
	generated, err := os.ReadFile(output)
	if err != nil || !strings.Contains(string(generated), `"gpt-native"`) {
		t.Fatalf("generated catalog = %s, %v", generated, err)
	}
	for name, script := range map[string]string{
		"nonzero": "#!/bin/sh\necho export failed >&2\nexit 7\n",
		"invalid": "#!/bin/sh\nprintf 'not JSON\\n'\n",
	} {
		t.Run(name, func(t *testing.T) {
			brokenExporter := filepath.Join(directory, "broken-"+name)
			if err := os.WriteFile(brokenExporter, []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			if code, _, _ := runCLI(t, "catalog", "generate", "--config", configPath, "--codex", brokenExporter); code != exitError {
				t.Fatalf("broken export exited %d, want %d", code, exitError)
			}
			after, err := os.ReadFile(output)
			if err != nil {
				t.Fatal(err)
			}
			if string(after) != string(generated) {
				t.Fatal("failed export replaced the final catalog")
			}
		})
	}
	missingRaw := filepath.Join(directory, "missing-native.json")
	missingRawConfig := strings.Replace(configText, `"output_file"`, `"native_catalog_file": "`+filepath.ToSlash(missingRaw)+`", "output_file"`, 1)
	if err := os.WriteFile(configPath, []byte(missingRawConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := runCLI(t, "catalog", "generate", "--config", configPath); code != exitError {
		t.Fatalf("missing explicit raw input exited %d, want %d", code, exitError)
	}
	after, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(generated) {
		t.Fatal("missing raw input replaced the final catalog")
	}

	// An explicit raw input remains offline and does not try to run --codex.
	raw := filepath.Join(directory, "native.json")
	if err := os.WriteFile(raw, generated, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte(strings.Replace(configText, `"output_file"`, `"native_catalog_file": "`+filepath.ToSlash(raw)+`", "output_file"`, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	mustRun(t, exitOK, "catalog", "generate", "--config", configPath, "--codex", filepath.Join(directory, "missing-codex"))
}

func TestSetupDryRunDoesNotWriteOrRunCodex(t *testing.T) {
	home := t.TempDir()
	configHome := t.TempDir()
	dataHome := t.TempDir()
	codexHome := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", configHome)
	t.Setenv("XDG_DATA_HOME", dataHome)
	t.Setenv("CODEX_HOME", codexHome)
	t.Setenv("CODEX_MODEL_ROUTER_CONFIG", "")
	marker := filepath.Join(home, "codex-ran")
	exporter := filepath.Join(home, "fake-codex")
	if err := os.WriteFile(exporter, []byte("#!/bin/sh\ntouch "+strconv.Quote(marker)+"\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	out := mustRun(t, exitOK, "setup", "--dry-run", "--configure-codex", "--codex", exporter)
	for _, want := range []string{"would create native-only", "dry run: no files"} {
		if !strings.Contains(out, want) {
			t.Fatalf("dry-run output lacks %q:\n%s", want, out)
		}
	}
	for _, path := range []string{
		filepath.Join(configHome, "codex-model-router", "config.json"),
		filepath.Join(dataHome, "codex-model-router", "catalog.json"),
		filepath.Join(codexHome, "config.toml"), marker,
	} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("dry run changed %s: %v", path, err)
		}
	}
	if code, _, _ := runCLI(t, "setup", "--unknown-flag"); code != exitUsage {
		t.Fatalf("unknown setup flag exited %d, want %d", code, exitUsage)
	}
}

func TestSetupConfiguresCodexOnlyAfterAHealthyService(t *testing.T) {
	work := newWorkspace(t)
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	codexPath := filepath.Join(codexHome, "config.toml")
	original := []byte("# keep this comment\n[plugins.example]\nenabled = true\n")
	if err := os.WriteFile(codexPath, original, 0o600); err != nil {
		t.Fatal(err)
	}
	serviceFake := &fakeSetupService{}
	previousFactory, previousHealth := setupServiceFactory, setupHealthWaiter
	setupServiceFactory = func(_ *config.Config, options service.Options) setupService {
		serviceFake.options = options
		return serviceFake
	}
	setupHealthWaiter = func(_ *config.Config, _ time.Duration) (map[string]any, error) {
		return map[string]any{"ok": true}, nil
	}
	defer func() {
		setupServiceFactory = previousFactory
		setupHealthWaiter = previousHealth
	}()

	mustRun(t, exitOK, "setup", "--config", work.configPath, "--bin", filepath.Join(work.dir, "router"), "--configure-codex")
	if serviceFake.installs != 1 {
		t.Fatalf("service install count = %d", serviceFake.installs)
	}
	if serviceFake.options.ConfigPath != work.configPath || serviceFake.options.BinaryPath != filepath.Join(work.dir, "router") {
		t.Fatalf("service options = %+v", serviceFake.options)
	}
	updated, err := os.ReadFile(codexPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(updated), `openai_base_url = "http://127.0.0.1:4317/v1"`) || !strings.Contains(string(updated), "[plugins.example]") {
		t.Fatalf("Codex configuration =\n%s", updated)
	}
	backup, err := os.ReadFile(codexPath + ".codex-model-router.bak")
	if err != nil {
		t.Fatal(err)
	}
	if string(backup) != string(original) {
		t.Fatal("Codex backup was not the exact pre-setup file")
	}
}

func TestSetupHealthFailureLeavesCodexConfigurationUntouched(t *testing.T) {
	work := newWorkspace(t)
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	codexPath := filepath.Join(codexHome, "config.toml")
	original := []byte("# do not change before health succeeds\n")
	if err := os.WriteFile(codexPath, original, 0o600); err != nil {
		t.Fatal(err)
	}
	serviceFake := &fakeSetupService{}
	previousFactory, previousHealth := setupServiceFactory, setupHealthWaiter
	setupServiceFactory = func(_ *config.Config, _ service.Options) setupService { return serviceFake }
	setupHealthWaiter = func(_ *config.Config, _ time.Duration) (map[string]any, error) {
		return nil, errors.New("not ready")
	}
	defer func() {
		setupServiceFactory = previousFactory
		setupHealthWaiter = previousHealth
	}()
	if code, _, _ := runCLI(t, "setup", "--config", work.configPath, "--bin", filepath.Join(work.dir, "router"), "--configure-codex"); code != exitError {
		t.Fatalf("unhealthy setup exited %d, want %d", code, exitError)
	}
	if serviceFake.installs != 1 {
		t.Fatalf("service install count = %d", serviceFake.installs)
	}
	after, err := os.ReadFile(codexPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(original) {
		t.Fatal("unhealthy service changed Codex configuration")
	}
	if _, err := os.Stat(codexPath + ".codex-model-router.bak"); !os.IsNotExist(err) {
		t.Fatalf("unhealthy service created a Codex backup: %v", err)
	}
}

func TestSetupCatalogFailureDoesNotInstallTheService(t *testing.T) {
	work := newWorkspace(t)
	data, err := os.ReadFile(work.configPath)
	if err != nil {
		t.Fatal(err)
	}
	broken := filepath.Join(work.dir, "missing-native.json")
	if err := os.WriteFile(work.configPath, []byte(strings.Replace(string(data), filepath.ToSlash(work.nativeCatalog), filepath.ToSlash(broken), 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	serviceFake := &fakeSetupService{}
	previousFactory := setupServiceFactory
	setupServiceFactory = func(_ *config.Config, _ service.Options) setupService { return serviceFake }
	defer func() { setupServiceFactory = previousFactory }()
	if code, _, _ := runCLI(t, "setup", "--config", work.configPath, "--bin", filepath.Join(work.dir, "router")); code != exitError {
		t.Fatalf("broken catalog setup exited %d, want %d", code, exitError)
	}
	if serviceFake.installs != 0 {
		t.Fatal("catalog failure disturbed the existing service")
	}
}

func TestSetupRequiresRouteCredentialsBeforeCatalogOrService(t *testing.T) {
	work := newWorkspace(t)
	configureRouteCredential(t, work, "REMOTE_KEY")
	serviceFake := &fakeSetupService{}
	previousFactory := setupServiceFactory
	setupServiceFactory = func(_ *config.Config, _ service.Options) setupService { return serviceFake }
	defer func() { setupServiceFactory = previousFactory }()

	for _, args := range [][]string{
		{"setup", "--config", work.configPath, "--bin", filepath.Join(work.dir, "router")},
		{"setup", "--dry-run", "--config", work.configPath, "--bin", filepath.Join(work.dir, "router")},
		{"setup", "--config", work.configPath, "--bin", filepath.Join(work.dir, "router"), "--env", "REMOTE_KEY="},
	} {
		code, _, stderr := runCLI(t, args...)
		if code != exitUsage || !strings.Contains(stderr, "--env REMOTE_KEY=VALUE") {
			t.Fatalf("%v exited %d with %q, want credential usage error", args, code, stderr)
		}
	}
	if serviceFake.installs != 0 || serviceFake.previews != 0 {
		t.Fatalf("missing credentials reached service setup: %+v", serviceFake)
	}
	if _, err := os.Stat(work.catalog); !os.IsNotExist(err) {
		t.Fatalf("missing credentials generated a catalog: %v", err)
	}
}

func TestSetupPassesRequiredRouteCredentialToService(t *testing.T) {
	work := newWorkspace(t)
	configureRouteCredential(t, work, "REMOTE_KEY")
	serviceFake := &fakeSetupService{}
	previousFactory, previousHealth := setupServiceFactory, setupHealthWaiter
	setupServiceFactory = func(_ *config.Config, options service.Options) setupService {
		serviceFake.options = options
		return serviceFake
	}
	setupHealthWaiter = func(_ *config.Config, _ time.Duration) (map[string]any, error) {
		return map[string]any{"ok": true}, nil
	}
	defer func() {
		setupServiceFactory = previousFactory
		setupHealthWaiter = previousHealth
	}()

	mustRun(t, exitOK, "setup", "--config", work.configPath, "--bin", filepath.Join(work.dir, "router"), "--env", "REMOTE_KEY=provided")
	if serviceFake.installs != 1 || serviceFake.options.Env["REMOTE_KEY"] != "provided" {
		t.Fatalf("service options = %+v, installs=%d", serviceFake.options, serviceFake.installs)
	}
}

func TestSetupDoesNotRequireNativeFallbackCredentialWithPassthrough(t *testing.T) {
	work := newWorkspace(t)
	configureNativeFallback(t, work, "NATIVE_FALLBACK_KEY")
	serviceFake := &fakeSetupService{}
	previousFactory, previousHealth := setupServiceFactory, setupHealthWaiter
	setupServiceFactory = func(_ *config.Config, _ service.Options) setupService { return serviceFake }
	setupHealthWaiter = func(_ *config.Config, _ time.Duration) (map[string]any, error) {
		return map[string]any{"ok": true}, nil
	}
	defer func() {
		setupServiceFactory = previousFactory
		setupHealthWaiter = previousHealth
	}()

	mustRun(t, exitOK, "setup", "--config", work.configPath, "--bin", filepath.Join(work.dir, "router"))
	if serviceFake.installs != 1 {
		t.Fatalf("service install count = %d", serviceFake.installs)
	}
}

func TestSetupRequiresNativeCredentialWhenPassthroughIsDisabled(t *testing.T) {
	work := newWorkspace(t)
	configureNativeCredentialRequirement(t, work, "NATIVE_KEY")
	serviceFake := &fakeSetupService{}
	previousFactory, previousHealth := setupServiceFactory, setupHealthWaiter
	setupServiceFactory = func(_ *config.Config, options service.Options) setupService {
		serviceFake.options = options
		return serviceFake
	}
	setupHealthWaiter = func(_ *config.Config, _ time.Duration) (map[string]any, error) {
		return map[string]any{"ok": true}, nil
	}
	defer func() {
		setupServiceFactory = previousFactory
		setupHealthWaiter = previousHealth
	}()

	if code, _, stderr := runCLI(t, "setup", "--config", work.configPath, "--bin", filepath.Join(work.dir, "router")); code != exitUsage || !strings.Contains(stderr, "--env NATIVE_KEY=VALUE") {
		t.Fatalf("missing native credential exited %d with %q", code, stderr)
	}
	if serviceFake.installs != 0 || serviceFake.previews != 0 {
		t.Fatalf("missing native credential reached service setup: %+v", serviceFake)
	}
	mustRun(t, exitOK, "setup", "--config", work.configPath, "--bin", filepath.Join(work.dir, "router"), "--env", "NATIVE_KEY=provided")
	if serviceFake.installs != 1 || serviceFake.options.Env["NATIVE_KEY"] != "provided" {
		t.Fatalf("service options = %+v, installs=%d", serviceFake.options, serviceFake.installs)
	}
}

func TestSetupPropagatesDefaultCatalogDataHomeToService(t *testing.T) {
	work := newWorkspace(t)
	configureDefaultCatalogPath(t, work)
	t.Setenv("HOME", t.TempDir())
	dataHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHome)
	serviceFake := &fakeSetupService{}
	previousFactory, previousHealth := setupServiceFactory, setupHealthWaiter
	setupServiceFactory = func(_ *config.Config, options service.Options) setupService {
		serviceFake.options = options
		return serviceFake
	}
	setupHealthWaiter = func(_ *config.Config, _ time.Duration) (map[string]any, error) {
		return map[string]any{"ok": true}, nil
	}
	defer func() {
		setupServiceFactory = previousFactory
		setupHealthWaiter = previousHealth
	}()

	mustRun(t, exitOK, "setup", "--config", work.configPath, "--bin", filepath.Join(work.dir, "router"))
	if serviceFake.options.Env["XDG_DATA_HOME"] != dataHome {
		t.Fatalf("service data home = %q, want %q", serviceFake.options.Env["XDG_DATA_HOME"], dataHome)
	}
	if _, err := os.Stat(filepath.Join(dataHome, "codex-model-router", "catalog.json")); err != nil {
		t.Fatalf("setup did not generate the catalog in the propagated data home: %v", err)
	}
}

func TestSetupValidatesExplicitCatalogDataHomeOverride(t *testing.T) {
	work := newWorkspace(t)
	configureDefaultCatalogPath(t, work)
	dataHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHome)
	serviceFake := &fakeSetupService{}
	previousFactory := setupServiceFactory
	setupServiceFactory = func(_ *config.Config, options service.Options) setupService {
		serviceFake.options = options
		return serviceFake
	}
	defer func() { setupServiceFactory = previousFactory }()

	mustRun(t, exitOK, "setup", "--dry-run", "--config", work.configPath, "--bin", filepath.Join(work.dir, "router"), "--env", "XDG_DATA_HOME="+dataHome)
	if serviceFake.previews != 1 || serviceFake.options.Env["XDG_DATA_HOME"] != dataHome {
		t.Fatalf("matching XDG_DATA_HOME was not preserved: %+v", serviceFake)
	}
	conflicting := t.TempDir()
	code, _, stderr := runCLI(t, "setup", "--dry-run", "--config", work.configPath, "--bin", filepath.Join(work.dir, "router"), "--env", "XDG_DATA_HOME="+conflicting)
	if code != exitUsage || !strings.Contains(stderr, "conflicts with the catalog data home") {
		t.Fatalf("conflicting XDG_DATA_HOME exited %d with %q", code, stderr)
	}
	if serviceFake.previews != 1 || serviceFake.installs != 0 {
		t.Fatalf("conflicting XDG_DATA_HOME reached service setup: %+v", serviceFake)
	}
}

func TestServicePreviewPropagatesDefaultCatalogDataHome(t *testing.T) {
	work := newWorkspace(t)
	configureDefaultCatalogPath(t, work)
	t.Setenv("HOME", t.TempDir())
	dataHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHome)
	binary := filepath.Join(work.dir, "router")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	out := mustRun(t, exitOK, "service", "preview", "--config", work.configPath, "--bin", binary)
	if !strings.Contains(out, "XDG_DATA_HOME") || !strings.Contains(out, dataHome) {
		t.Fatalf("service preview omitted effective XDG data home:\n%s", out)
	}
}

func TestSetupDryRunIgnoresRelativeXDGConfigHomeOnLinux(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("systemd unit path applies on Linux")
	}
	work := newWorkspace(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", "relative-config")
	mustRun(t, exitOK, "setup", "--dry-run", "--config", work.configPath, "--bin", filepath.Join(work.dir, "router"))
}

func TestDefaultConfigPathHonorsOnlyAbsoluteXDGValues(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_MODEL_ROUTER_CONFIG", "")
	absolute := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", absolute)
	path, err := defaultConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	if path != filepath.Join(absolute, "codex-model-router", "config.json") {
		t.Fatalf("absolute XDG config path = %q", path)
	}
	t.Setenv("XDG_CONFIG_HOME", "relative-config")
	path, err = defaultConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	if path != filepath.Join(home, ".config", "codex-model-router", "config.json") {
		t.Fatalf("relative XDG config path = %q", path)
	}
}

func TestServicePreviewAndDryRunTouchNothing(t *testing.T) {
	work := newWorkspace(t)
	t.Setenv("HOME", t.TempDir())
	binary := filepath.Join(work.dir, "codex-model-router")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	out := mustRun(t, exitOK, "service", "preview", "--config", work.configPath, "--bin", binary)
	marker := "<key>Label</key>"
	pathMarker := "# plist path:"
	if runtime.GOOS == "linux" {
		marker = "[Service]"
		pathMarker = "# unit path:"
	}
	if !strings.Contains(out, marker) || !strings.Contains(out, service.DefaultLabel) {
		t.Fatalf("preview output:\n%s", out)
	}
	if !strings.Contains(out, "serve") || !strings.Contains(out, work.configPath) {
		t.Fatalf("preview does not show the serve invocation:\n%s", out)
	}
	if !strings.Contains(out, pathMarker) {
		t.Fatalf("preview does not report where the plist would go:\n%s", out)
	}
	// Nothing is written by preview.
	plistDir := filepath.Join(os.Getenv("HOME"), "Library", "LaunchAgents")
	if runtime.GOOS == "linux" {
		plistDir = filepath.Join(os.Getenv("HOME"), ".config", "systemd", "user")
	}
	if _, err := os.Stat(plistDir); !os.IsNotExist(err) {
		t.Fatal("service preview created the LaunchAgents directory")
	}

	mustRun(t, exitOK, "service", "install", "--dry-run", "--config", work.configPath, "--bin", binary)
	if _, err := os.Stat(plistDir); !os.IsNotExist(err) {
		t.Fatal("a dry run created the LaunchAgents directory")
	}
	if _, err := os.Stat(filepath.Join(plistDir, service.DefaultLabel+".plist")); !os.IsNotExist(err) {
		t.Fatal("a dry run wrote a plist")
	}
	mustRun(t, exitOK, "service", "uninstall", "--dry-run", "--config", work.configPath, "--bin", binary)

	if code, _, _ := runCLI(t, "service", "preview", "--config", work.configPath, "--bin", binary, "--env", "no-equals-sign"); code != exitUsage {
		t.Fatalf("a malformed --env exited %d, want %d", code, exitUsage)
	}
	if code, _, _ := runCLI(t, "service", "preview", "--config", work.configPath, "--bin", binary, "--shutdown-timeout", "nonsense"); code != exitUsage {
		t.Fatalf("a malformed --shutdown-timeout exited %d, want %d", code, exitUsage)
	}
	if code, _, _ := runCLI(t, "service", "bogus"); code != exitUsage {
		t.Fatalf("an unknown service subcommand exited %d", code)
	}
}

func TestServiceStatusReportsNothingInstalled(t *testing.T) {
	work := newWorkspace(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	binary := filepath.Join(work.dir, "codex-model-router")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// The label is test-only, so the real LaunchAgents directory is never consulted.
	out := mustRun(t, exitOK, "service", "status", "--config", work.configPath, "--bin", binary,
		"--label", "test.codex-model-router.status-check")
	if !strings.Contains(out, "not installed") {
		t.Fatalf("status output:\n%s", out)
	}
}

func TestHealthcheckAgainstALiveRouter(t *testing.T) {
	work := newWorkspace(t)
	cfg, err := config.Load(work.configPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := catalog.Generate(cfg, ""); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(cfg.Catalog.NativeCatalogFile); err != nil {
		t.Fatal(err)
	}
	cfg, err = config.Load(work.configPath)
	if err != nil {
		t.Fatal(err)
	}

	router, err := routing.New(cfg, routing.Options{Logger: newLogger(cfg, "error", os.Stderr), Env: os.LookupEnv})
	if err != nil {
		t.Fatal(err)
	}
	// Port 0 asks the kernel for a free port; the real value comes back from Addr.
	server, err := serve.New(router, serve.Options{Host: "127.0.0.1", Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	router.SetBoundAddr(server.Addr().String())
	go func() { _ = server.Serve() }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
	})
	port := strconv.Itoa(server.Addr().(*net.TCPAddr).Port)

	out := mustRun(t, exitOK, "healthcheck", "--config", work.configPath, "--port", port)
	if !strings.Contains(out, "healthy") {
		t.Fatalf("healthcheck output: %s", out)
	}
	jsonOut := mustRun(t, exitOK, "healthcheck", "--config", work.configPath, "--port", port, "--json")
	var health map[string]any
	if err := json.Unmarshal([]byte(jsonOut), &health); err != nil {
		t.Fatalf("--json is not JSON: %v\n%s", err, jsonOut)
	}
	if health["ok"] != true {
		t.Fatalf("health document = %v", health)
	}

	// An unreachable port fails, which is what a supervisor needs to see.
	if code, _, _ := runCLI(t, "healthcheck", "--config", work.configPath, "--port", "1", "--timeout", "300ms"); code != exitError {
		t.Fatalf("an unreachable router exited %d, want %d", code, exitError)
	}

	// The same wiring serves real traffic: a routed model reaches its upstream and
	// a native model reaches the native upstream.
	requestBody := strings.NewReader(`{"model":"qwen3-32b","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`)
	response, err := http.Post("http://127.0.0.1:"+port+"/v1/responses", "application/json", requestBody)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("routed request status = %d", response.StatusCode)
	}
	if work.calls.count() != 1 {
		t.Fatalf("upstream calls = %d", work.calls.count())
	}
}

func TestRandomPortHealthcheckNeedsAnExplicitPort(t *testing.T) {
	work := newWorkspace(t)
	random := filepath.Join(work.dir, "random-port.json")
	data, err := os.ReadFile(work.configPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(random, []byte(strings.Replace(string(data), `"port": 4317`, `"port": 0`, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := runCLI(t, "healthcheck", "--config", random); code != exitUsage {
		t.Fatalf("a random-port healthcheck exited %d, want %d", code, exitUsage)
	}
	// validate does not need a stable URL.
	mustRun(t, exitOK, "validate", "--config", random)
}

func TestServeReportsAnInvalidConfiguration(t *testing.T) {
	work := newWorkspace(t)
	// The router must refuse a configuration before opening a socket, so this test
	// must never hand serve a valid one: a valid configuration would run forever.
	broken := filepath.Join(work.dir, "broken.json")
	if err := os.WriteFile(broken, []byte(`{"listen":{"host":"0.0.0.0","port":4317}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := runCLI(t, "serve", "--config", broken)
	if code != exitConfig {
		t.Fatalf("serve with an invalid configuration exited %d, want %d: %s", code, exitConfig, stderr)
	}
	if !strings.Contains(stderr, "loopback") {
		t.Fatalf("the refusal did not say why: %s", stderr)
	}

	// A bad flag is a usage error, and it is caught before the file is read.
	for _, args := range [][]string{
		{"serve", "--config", broken, "--shutdown-timeout", "nonsense"},
		{"serve", "--config"},
		{"serve", "--unknown-flag"},
		{"serve", "extra-position"},
	} {
		if code, _, _ := runCLI(t, args...); code != exitUsage {
			t.Fatalf("%v exited %d, want %d", args, code, exitUsage)
		}
	}
}

func TestConfigurationIsFoundThroughTheEnvironment(t *testing.T) {
	work := newWorkspace(t)
	t.Setenv("CODEX_MODEL_ROUTER_CONFIG", work.configPath)
	mustRun(t, exitOK, "validate")
	t.Setenv("CODEX_MODEL_ROUTER_CONFIG", filepath.Join(work.dir, "absent.json"))
	if code, _, _ := runCLI(t, "validate"); code != exitConfig {
		t.Fatalf("a missing env configuration exited %d, want %d", code, exitConfig)
	}
	// An explicit path still takes precedence over the environment override.
	mustRun(t, exitOK, "validate", "--config", work.configPath)
}

func TestInstalledConfigurationAndCatalog(t *testing.T) {
	work := newWorkspace(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_MODEL_ROUTER_CONFIG", "")
	t.Setenv("CODEX_MODEL_ROUTER_BIN", "")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Chdir(t.TempDir())
	directory := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "codex-model-router")
	path := filepath.Join(directory, "config.json")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(work.configPath)
	if err != nil {
		t.Fatal(err)
	}
	data = []byte(strings.Replace(string(data), `"output_file": "catalog.json"`, `"output_file": ""`, 1))
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	mustRun(t, exitOK, "validate")
	mustRun(t, exitOK, "catalog", "generate")
	generated := filepath.Join(os.Getenv("XDG_DATA_HOME"), "codex-model-router", "catalog.json")
	catalogData, err := os.ReadFile(generated)
	if err != nil {
		t.Fatal(err)
	}
	if count, err := catalog.CountModels(catalogData); err != nil || count != 2 {
		t.Fatalf("installed catalog has %d entries: %v", count, err)
	}
	preview := mustRun(t, exitOK, "service", "preview")
	if !strings.Contains(preview, path) || !strings.Contains(preview, filepath.Join(home, ".local", "bin", "codex-model-router")) {
		t.Fatalf("service does not use the installed runtime paths:\n%s", preview)
	}
}

func TestRedactAttributeHidesSecrets(t *testing.T) {
	for _, key := range []string{"api_key", "Authorization", "x_api_key", "cookie", "upstream_header", "password", "token"} {
		redacted := redactAttribute(nil, attrOf(key, "super-secret"))
		if redacted.Value.String() != "[redacted]" {
			t.Fatalf("%s was logged in the clear: %s", key, redacted.Value.String())
		}
	}
	kept := redactAttribute(nil, attrOf("model", "qwen3-32b"))
	if kept.Value.String() != "qwen3-32b" {
		t.Fatalf("an ordinary attribute was redacted: %s", kept.Value.String())
	}
}

// attrOf builds a log attribute for the redaction test.
func attrOf(key, value string) slog.Attr { return slog.String(key, value) }
