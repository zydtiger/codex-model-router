package config_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/zydtiger/codex-model-router/internal/config"
)

// minimal returns a configuration that is valid with one route and one native
// model. Tests mutate the returned text so each case names exactly what differs.
func minimal(t *testing.T) string {
	t.Helper()
	return `{
	  "listen": {"host": "127.0.0.1", "port": 4317},
	  "native": {
	    "chatgpt_base_url": "https://native.invalid/backend-api/codex",
	    "api_base_url": "https://native.invalid/v1",
	    "models": ["gpt-5"]
	  },
	  "routes": [{"name": "local", "base_url": "http://127.0.0.1:30000/v1", "models": ["qwen3"]}]
	}`
}

func parse(t *testing.T, text string) *config.Config {
	t.Helper()
	cfg, err := config.Parse([]byte(text))
	if err != nil {
		t.Fatalf("parse: %v\n%s", err, text)
	}
	return cfg
}

func parseError(t *testing.T, text string) string {
	t.Helper()
	_, err := config.Parse([]byte(text))
	if err == nil {
		t.Fatalf("expected a configuration error for:\n%s", text)
	}
	return err.Error()
}

func TestMinimalConfigCompilesRouting(t *testing.T) {
	cfg := parse(t, minimal(t))
	route, ok := cfg.RouteFor("qwen3")
	if !ok || route.Name != "local" {
		t.Fatalf("RouteFor(qwen3) = %v, %v", route, ok)
	}
	if !cfg.IsNativeModel("gpt-5") {
		t.Fatal("gpt-5 should be an allowed native model")
	}
	if cfg.IsNativeModel("qwen3") {
		t.Fatal("a routed model must not also count as native")
	}
	if cfg.IsNativeModel("gpt-9") {
		t.Fatal("an unlisted model must not be allowed natively")
	}
	if got := cfg.BaseURL(); got != "http://127.0.0.1:4317/v1" {
		t.Fatalf("BaseURL = %q", got)
	}
	if cfg.MaxRequestBytes != config.DefaultMaxRequestByte {
		t.Fatalf("max_request_bytes default = %d", cfg.MaxRequestBytes)
	}
	if !cfg.PreserveClientAuth() {
		t.Fatal("preserve_client_auth should default to true")
	}
}

func TestExampleConfigurationIsValid(t *testing.T) {
	cfg, err := config.ExampleValidated()
	if err != nil {
		t.Fatalf("the shipped example must be a valid configuration: %v", err)
	}
	if _, ok := cfg.RouteFor("qwen3-32b"); !ok {
		t.Fatal("the example should route its documented model")
	}
	for _, route := range cfg.Routes {
		if strings.HasPrefix(route.BaseURL, "https://") {
			t.Fatalf("the example should stay on localhost, got %s", route.BaseURL)
		}
	}
}

func TestExampleFileMatchesEmbeddedCopy(t *testing.T) {
	embedded, err := config.Example()
	if err != nil {
		t.Fatal(err)
	}
	onDisk, err := os.ReadFile(filepath.Join("example", "config.example.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(embedded) != string(onDisk) {
		t.Fatal("the embedded example and the repository example differ")
	}
}

func TestListenRejectsNonLoopback(t *testing.T) {
	for _, host := range []string{"0.0.0.0", "192.168.1.20", "router.example", "::"} {
		text := strings.Replace(minimal(t), `"host": "127.0.0.1"`, `"host": "`+host+`"`, 1)
		if message := parseError(t, text); !strings.Contains(message, "loopback") {
			t.Fatalf("host %q error = %s, want a loopback refusal", host, message)
		}
	}
	for _, host := range []string{"localhost", "127.0.0.53", "::1"} {
		text := strings.Replace(minimal(t), `"host": "127.0.0.1"`, `"host": "`+host+`"`, 1)
		if cfg := parse(t, text); cfg.Listen.Host == "" {
			t.Fatalf("host %q should be accepted", host)
		}
	}
}

func TestNativeUpstreamMustBeHTTPSWhenPreservingClientAuth(t *testing.T) {
	// The default forwards the caller's credential, so a plaintext upstream on a
	// routable host would leak it.
	text := strings.Replace(minimal(t), "https://native.invalid", "http://native.invalid", 1)
	if message := parseError(t, text); !strings.Contains(message, "https") {
		t.Fatalf("error = %s, want an https requirement", message)
	}

	// A loopback upstream is allowed: it is how a local test server is addressed,
	// and the credential cannot leave the machine.
	localhost := strings.Replace(minimal(t), `"preserve_client_auth": true,`, ``, 1)
	localhost = strings.Replace(localhost, "https://native.invalid", "http://127.0.0.1:9", 1)
	parse(t, localhost)

	// Turning preservation off removes the requirement, because the router then
	// sends no client credential at all.
	off := strings.Replace(minimal(t), `"native": {`, `"native": {"preserve_client_auth": false,`, 1)
	off = strings.Replace(off, "https://native.invalid", "http://router.internal", 1)
	cfg := parse(t, off)
	if cfg.PreserveClientAuth() {
		t.Fatal("preserve_client_auth should be false")
	}
}

func TestUnknownFieldsAreRejected(t *testing.T) {
	// A typo in a security-relevant key must fail loudly rather than leave the
	// default in place.
	text := strings.Replace(minimal(t), `"base_path"`, `"base_path"`, 1)
	text = strings.Replace(text, `{
	  "listen"`, `{
	  "base_pathh": "/v1",
	  "listen"`, 1)
	if text == minimal(t) {
		text = `{"unknown_key": true, ` + minimal(t)[1:]
	}
	if message := parseError(t, text); !strings.Contains(message, "unknown") && !strings.Contains(message, "base_pathh") {
		t.Fatalf("error = %s, want an unknown field complaint", message)
	}

	text = strings.Replace(minimal(t), `"models": ["qwen3"]`, `"models": ["qwen3"], "route_prefix": "/x"`, 1)
	if message := parseError(t, text); !strings.Contains(message, "route_prefix") {
		t.Fatalf("error = %s, want an unknown route field complaint", message)
	}
}

func TestDuplicateAndEmptyModelIDs(t *testing.T) {
	text := strings.Replace(minimal(t), `"models": ["qwen3"]`, `"models": ["qwen3", "qwen3"]`, 1)
	// A duplicate inside one route is collapsed rather than rejected, because it
	// cannot create an ambiguous lookup.
	parse(t, text)

	text = strings.Replace(minimal(t), `"name": "local"`, `"name": "local"`, 1) + ``
	duplicated := strings.Replace(minimal(t),
		`"routes": [{"name": "local", "base_url": "http://127.0.0.1:30000/v1", "models": ["qwen3"]}]`,
		`"routes": [
		   {"name": "a", "base_url": "http://127.0.0.1:30000/v1", "models": ["qwen3"]},
		   {"name": "b", "base_url": "http://127.0.0.1:30001/v1", "models": ["qwen3"]}
		 ]`, 1)
	if message := parseError(t, duplicated); !strings.Contains(message, "already routed") {
		t.Fatalf("error = %s, want a duplicate route complaint", message)
	}

	emptyNames := strings.Replace(minimal(t), `"models": ["qwen3"]`, `"models": ["  "]`, 1)
	if message := parseError(t, emptyNames); !strings.Contains(message, "at least one model") {
		t.Fatalf("error = %s, want a missing-model complaint", message)
	}
}

func TestModelIDsWithWhitespaceAreRejected(t *testing.T) {
	text := strings.Replace(minimal(t), `"models": ["qwen3"]`, `"models": ["qwen 3"]`, 1)
	if message := parseError(t, text); !strings.Contains(message, "whitespace") {
		t.Fatalf("error = %s, want a whitespace complaint", message)
	}
}

func TestUpstreamURLsCannotSmuggleCredentialsOrQueries(t *testing.T) {
	for _, base := range []string{
		"http://user:pass@127.0.0.1:30000/v1",
		"http://127.0.0.1:30000/v1?target=evil",
		"ftp://127.0.0.1:30000/v1",
		"//127.0.0.1:30000/v1",
	} {
		text := strings.Replace(minimal(t), "http://127.0.0.1:30000/v1", base, 1)
		if message := parseError(t, text); message == "" {
			t.Fatalf("base URL %q should be refused", base)
		}
	}
}

func TestExtraHeadersCannotCarryCredentials(t *testing.T) {
	for _, header := range []string{"Authorization", "Cookie", "X-Forwarded-For", "ChatGPT-Account-ID", "Content-Length", "Host"} {
		text := strings.Replace(minimal(t), `"models": ["qwen3"]`, `"models": ["qwen3"], "extra_headers": {"`+header+`": "x"}`, 1)
		if message := parseError(t, text); !strings.Contains(message, "extra_headers") {
			t.Fatalf("header %q error = %s, want an extra_headers refusal", header, message)
		}
	}
	// A route credential is allowed on Authorization, and comes from the environment.
	text := strings.Replace(minimal(t), `"models": ["qwen3"]`, `"models": ["qwen3"], "auth": {"api_key_env": "LOCAL_KEY", "scheme": "bearer"}`, 1)
	parse(t, text)
	for _, header := range []string{"Cookie", "ChatGPT-Account-ID", "Host"} {
		credentialText := strings.Replace(text, `"scheme": "bearer"`, `"scheme": "bearer", "header": "`+header+`"`, 1)
		if message := parseError(t, credentialText); !strings.Contains(message, "credential") {
			t.Fatalf("credential header %q error = %s", header, message)
		}
	}
}

func TestAuthSchemeAndEnvNameValidation(t *testing.T) {
	for _, name := range []string{"_SGLANG_API_KEY", "_", "KEY_1"} {
		text := strings.Replace(minimal(t), `"models": ["qwen3"]`, `"models": ["qwen3"], "auth": {"api_key_env": "`+name+`"}`, 1)
		parse(t, text)
	}
	for _, name := range []string{"1KEY", "KEY-ONE", "KEY=VALUE"} {
		text := strings.Replace(minimal(t), `"models": ["qwen3"]`, `"models": ["qwen3"], "auth": {"api_key_env": "`+name+`"}`, 1)
		if message := parseError(t, text); !strings.Contains(message, "api_key_env") {
			t.Fatalf("environment name %q error = %s", name, message)
		}
	}
	text := strings.Replace(minimal(t), `"models": ["qwen3"]`, `"models": ["qwen3"], "auth": {"api_key_env": "not an env"}`, 1)
	if message := parseError(t, text); !strings.Contains(message, "api_key_env") {
		t.Fatalf("error = %s", message)
	}
	text = strings.Replace(minimal(t), `"models": ["qwen3"]`, `"models": ["qwen3"], "auth": {"api_key_env": "K", "scheme": "query"}`, 1)
	if message := parseError(t, text); !strings.Contains(message, "scheme") {
		t.Fatalf("error = %s", message)
	}
	text = strings.Replace(minimal(t), `"models": ["qwen3"]`, `"models": ["qwen3"], "auth": {"api_key_env": "K", "scheme": "header", "header": "Authorization"}`, 1)
	if message := parseError(t, text); !strings.Contains(message, "bearer") {
		t.Fatalf("error = %s, want guidance to use bearer", message)
	}
}

func TestReasoningAdapterValidation(t *testing.T) {
	missing := strings.Replace(minimal(t), `"models": ["qwen3"]`, `"models": ["qwen3"], "reasoning": {"adapter": "sglang_chat_template"}`, 1)
	if message := parseError(t, missing); !strings.Contains(message, "chat_template_kwargs") {
		t.Fatalf("error = %s", message)
	}
	unknown := strings.Replace(minimal(t), `"models": ["qwen3"]`, `"models": ["qwen3"], "reasoning": {"adapter": "mystery"}`, 1)
	if message := parseError(t, unknown); !strings.Contains(message, "adapter") {
		t.Fatalf("error = %s", message)
	}
	nested := strings.Replace(minimal(t), `"models": ["qwen3"]`,
		`"models": ["qwen3"], "reasoning": {"adapter": "sglang_chat_template", "chat_template_kwargs": {"enable_thinking": {"low": {"nested": true}}}}`, 1)
	if message := parseError(t, nested); !strings.Contains(message, "scalar") {
		t.Fatalf("error = %s, want a scalar-value complaint", message)
	}
	badPolicy := strings.Replace(minimal(t), `"models": ["qwen3"]`,
		`"models": ["qwen3"], "reasoning": {"adapter": "none", "unknown_effort": "guess"}`, 1)
	if message := parseError(t, badPolicy); !strings.Contains(message, "unknown_effort") {
		t.Fatalf("error = %s", message)
	}
}

func TestInputPoliciesAreValidated(t *testing.T) {
	text := strings.Replace(minimal(t), `"models": ["qwen3"]`, `"models": ["qwen3"], "input": {"reasoning_items": "explode"}`, 1)
	if message := parseError(t, text); !strings.Contains(message, "reasoning_items") {
		t.Fatalf("error = %s", message)
	}
	cfg := parse(t, strings.Replace(minimal(t), `"models": ["qwen3"]`, `"models": ["qwen3"], "input": {"custom_tools": "keep"}`, 1))
	if cfg.Routes[0].Input.CustomTools != "keep" {
		t.Fatalf("custom_tools = %q", cfg.Routes[0].Input.CustomTools)
	}
	if cfg.Routes[0].Input.ReasoningItems != config.PolicyDrop {
		t.Fatalf("reasoning_items default = %q, want drop", cfg.Routes[0].Input.ReasoningItems)
	}
	if !cfg.Routes[0].DeveloperRoleAsSystem() {
		t.Fatal("developer_role_as_system should default to true")
	}
}

func TestCatalogEntriesMustReferenceAConfiguredRoute(t *testing.T) {
	text := strings.Replace(minimal(t), `"routes":`, `"catalog": {"models": [{"id": "qwen3-extra", "route": "missing"}]}, "routes":`, 1)
	if message := parseError(t, text); !strings.Contains(message, `route "missing" is not defined`) {
		t.Fatalf("error = %s", message)
	}
	// A catalog entry also makes the model routable, which is what keeps the picker
	// and the router in step.
	text = strings.Replace(minimal(t), `"routes":`, `"catalog": {"models": [{"id": "qwen3-extra", "route": "local"}]}, "routes":`, 1)
	cfg := parse(t, text)
	if _, ok := cfg.RouteFor("qwen3-extra"); !ok {
		t.Fatal("a catalog entry should be routable through its route")
	}
}

func TestCatalogNativeCollisionIsRefused(t *testing.T) {
	directory := t.TempDir()
	native := filepath.Join(directory, "native.json")
	if err := os.WriteFile(native, []byte(`{"models":[{"slug":"gpt-5"},{"slug":"gpt-4"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	text := strings.Replace(minimal(t), `"listen"`, `"catalog": {"native_catalog_file": "`+filepath.ToSlash(native)+`"}, "listen"`, 1)
	cfg := parse(t, text)
	if !cfg.IsNativeModel("gpt-4") {
		t.Fatalf("native ids from the catalog should be allowed: %v", cfg.NativeModelIDs())
	}

	// Routing a model that the native catalog says is native would shadow a bundled
	// model, so it is refused.
	clash := strings.Replace(text, `"models": ["qwen3"]`, `"models": ["gpt-4"]`, 1)
	if message := parseError(t, clash); !strings.Contains(message, "also routed remotely") {
		t.Fatalf("error = %s", message)
	}
}

func TestCatalogNativeFileMustExist(t *testing.T) {
	text := strings.Replace(minimal(t), `"listen"`, `"catalog": {"native_catalog_file": "/nonexistent/native.json"}, "listen"`, 1)
	if message := parseError(t, text); !strings.Contains(message, "native_catalog_file") {
		t.Fatalf("error = %s", message)
	}
}

func TestDuplicateNativeModelEntriesAreIgnored(t *testing.T) {
	cfg := parse(t, strings.Replace(minimal(t), `"models": ["gpt-5"]`, `"models": ["gpt-5", " gpt-5 ", "gpt-5"]`, 1))
	if len(cfg.NativeModelIDs()) != 1 {
		t.Fatalf("native models = %v", cfg.NativeModelIDs())
	}
}

func TestNothingConfiguredIsAnError(t *testing.T) {
	text := `{"listen": {"host": "127.0.0.1", "port": 4317}, "routes": [], "native": {"models": []}}`
	if message := parseError(t, text); !strings.Contains(message, "no models are configured") {
		t.Fatalf("error = %s", message)
	}
}

func TestEmptyAndTrailingJSON(t *testing.T) {
	if message := parseError(t, "   "); !strings.Contains(message, "empty") {
		t.Fatalf("error = %s", message)
	}
	if message := parseError(t, minimal(t)+"\n{}"); !strings.Contains(message, "more than one JSON value") {
		t.Fatalf("error = %s", message)
	}
}

func TestLoadReportsPath(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "config.json")
	if err := os.WriteFile(path, []byte(`{"listen": {"host": "0.0.0.0"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := config.Load(path)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), path) {
		t.Fatalf("error should name the file: %v", err)
	}
}

func TestHomePathsExpand(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	text := strings.Replace(minimal(t), `"listen"`, `"catalog": {"output_file": "~/Library/Caches/catalog.json"}, "listen"`, 1)
	cfg := parse(t, text)
	if cfg.Catalog.OutputFile != filepath.Join(home, "Library/Caches/catalog.json") {
		t.Fatalf("output_file = %q", cfg.Catalog.OutputFile)
	}
}

func TestBasePathAndAddr(t *testing.T) {
	cfg := parse(t, strings.Replace(minimal(t), `"listen": {`, `"base_path": "/router/v1", "listen": {`, 1))
	if cfg.BasePath != "/router/v1" {
		t.Fatalf("base_path = %q", cfg.BasePath)
	}
	if cfg.BaseURL() != "http://127.0.0.1:4317/router/v1" {
		t.Fatalf("BaseURL = %q", cfg.BaseURL())
	}
	if cfg.Addr() != "127.0.0.1:4317" {
		t.Fatalf("Addr = %q", cfg.Addr())
	}
	if message := parseError(t, strings.Replace(minimal(t), `"listen": {`, `"base_path": "v1", "listen": {`, 1)); !strings.Contains(message, "must start with /") {
		t.Fatalf("error = %s", message)
	}
	if message := parseError(t, strings.Replace(minimal(t), `"listen": {`, `"base_path": "/../x", "listen": {`, 1)); !strings.Contains(message, "plain path") {
		t.Fatalf("error = %s", message)
	}
}

func TestIPv6LoopbackConfig(t *testing.T) {
	cfg := parse(t, strings.Replace(minimal(t), `"host": "127.0.0.1"`, `"host": "::1"`, 1))
	if cfg.Addr() != "[::1]:4317" {
		t.Fatalf("Addr = %q", cfg.Addr())
	}
	if !strings.HasPrefix(cfg.BaseURL(), "http://[::1]:4317") {
		t.Fatalf("BaseURL = %q", cfg.BaseURL())
	}
}

func TestHeaderAllowListIsExplicit(t *testing.T) {
	allowed := config.AllowedRequestHeaders()
	joined := strings.Join(allowed, ",")
	for _, want := range []string{"Accept", "Content-Type", "User-Agent"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("allow list is missing %s: %v", want, allowed)
		}
	}
	for _, forbidden := range []string{"Authorization", "Cookie", "Chatgpt-Account-Id", "X-Api-Key"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("allow list must not include %s: %v", forbidden, allowed)
		}
	}
}

func TestConfigRoundTripsThroughJSONTags(t *testing.T) {
	// The example is the contract for the field names, so decoding it into the
	// struct must not silently drop anything.
	example, err := config.Example()
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(example, &fields); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"listen", "base_path", "max_request_bytes", "log", "native", "routes", "catalog"} {
		if _, ok := fields[key]; !ok {
			t.Fatalf("the example should document %s", key)
		}
	}
}

func TestRelativePathsResolveAgainstTheConfigFile(t *testing.T) {
	directory := t.TempDir()
	// The process's working directory is somewhere else, so a path resolved against
	// it would point at a file that does not exist.
	other := t.TempDir()
	if previous, err := os.Getwd(); err == nil {
		t.Cleanup(func() { _ = os.Chdir(previous) })
		if err := os.Chdir(other); err != nil {
			t.Fatal(err)
		}
	}
	// The referenced files have to exist for the configuration to validate, and they
	// are created relative to the config directory: that is the whole point.
	if err := os.MkdirAll(filepath.Join(directory, "generated"), 0o755); err != nil {
		t.Fatal(err)
	}
	native := []byte(`{"models":[{"slug":"gpt-5","visibility":"list","supported_in_api":true}]}`)
	if err := os.WriteFile(filepath.Join(directory, "generated", "models.json"), native, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "instructions.md"), []byte("instructions"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A relative path written in the file is a path in the file's directory.
	path := filepath.Join(directory, "router.json")
	body := `{
	  "listen": {"host": "127.0.0.1", "port": 4317},
	  "native": {"chatgpt_base_url": "https://chatgpt.com/backend", "api_base_url": "https://api.openai.com/v1", "models": ["gpt-5"]},
	  "catalog": {
	    "native_catalog_file": "generated/models.json",
	    "output_file": "generated/router-models.json",
	    "base_instructions_file": "instructions.md",
	    "models": [{"id": "qwen3-32b", "route": "local"}]
	  },
	  "routes": [{"name": "local", "base_url": "http://127.0.0.1:30000/v1", "models": ["qwen3-32b"]}]
	}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	for name, got := range map[string]string{
		"native_catalog_file":    cfg.Catalog.NativeCatalogFile,
		"output_file":            cfg.Catalog.OutputFile,
		"base_instructions_file": cfg.Catalog.BaseInstructionsFile,
	} {
		if !filepath.IsAbs(got) {
			t.Fatalf("%s = %q, want an absolute path", name, got)
		}
		if !strings.HasPrefix(got, directory+string(filepath.Separator)) {
			t.Fatalf("%s = %q, want it under %s (the config file's directory, not the working directory)", name, got, directory)
		}
	}
	if got, want := cfg.Catalog.NativeCatalogFile, filepath.Join(directory, "generated", "models.json"); got != want {
		t.Fatalf("native_catalog_file = %q, want %q", got, want)
	}

	// An absolute path stays exactly as written.
	absolute := filepath.Join(other, "models.json")
	if err := os.WriteFile(absolute, native, 0o600); err != nil {
		t.Fatal(err)
	}
	body = strings.Replace(body, `"native_catalog_file": "generated/models.json"`, `"native_catalog_file": `+strconv.Quote(absolute), 1)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err = config.Load(path)
	if err != nil {
		t.Fatalf("load with an absolute path: %v", err)
	}
	if cfg.Catalog.NativeCatalogFile != absolute {
		t.Fatalf("NativeCatalogFile = %q, want %q", cfg.Catalog.NativeCatalogFile, absolute)
	}

	// Bytes that were not read from a file have no base directory, so a relative path
	// is left as written rather than pointed at an unrelated place.
	parsed, err := config.Parse([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Catalog.OutputFile != "generated/router-models.json" {
		t.Fatalf("Parse changed a relative path: %q", parsed.Catalog.OutputFile)
	}
}
