package main

import (
	"bytes"
	"context"
	"encoding/json"
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
	dir        string
	configPath string
	catalog    string
	upstream   string
	calls      *callLog
}

type callLog struct {
	mu    sync.Mutex
	paths []string
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
	configPath := filepath.Join(dir, "router.json")
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
    "output_file": "` + filepath.ToSlash(filepath.Join(dir, "model-catalog.json")) + `",
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
      "schema_loading": "on_demand"
    }
  }]
}`
	if err := os.WriteFile(configPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return &workspace{
		dir:        dir,
		configPath: configPath,
		catalog:    filepath.Join(dir, "model-catalog.json"),
		upstream:   upstream.URL,
		calls:      calls,
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
}

func TestUsageListsEverySubcommand(t *testing.T) {
	out := mustRun(t, exitOK, "help")
	for _, name := range []string{"serve", "validate", "catalog generate", "catalog print-example",
		"service preview", "service install", "service status", "service uninstall", "healthcheck", "version"} {
		if !strings.Contains(out, name) {
			t.Fatalf("usage does not mention %q:\n%s", name, out)
		}
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
