package catalog_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/zydtiger/codex-model-router/internal/catalog"
	"github.com/zydtiger/codex-model-router/internal/config"
)

func testConfigPath(t *testing.T, name string) string {
	t.Helper()
	path, err := filepath.Abs(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return path
}

// build parses a configuration, generates the catalog, and returns the decoded
// entries by slug.
func build(t *testing.T, configText string) (map[string]map[string]json.RawMessage, []byte) {
	t.Helper()
	cfg, err := config.Parse([]byte(configText))
	if err != nil {
		t.Fatalf("config: %v\n%s", err, configText)
	}
	data, err := catalog.Build(cfg)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var decoded struct {
		Models []map[string]json.RawMessage `json:"models"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("generated catalog is not valid: %v", err)
	}
	bySlug := map[string]map[string]json.RawMessage{}
	for _, entry := range decoded.Models {
		var slug string
		if err := json.Unmarshal(entry["slug"], &slug); err != nil {
			t.Fatalf("entry without a usable slug: %v", err)
		}
		if _, duplicate := bySlug[slug]; duplicate {
			t.Fatalf("duplicate slug %q in the generated catalog", slug)
		}
		bySlug[slug] = entry
	}
	return bySlug, data
}

func nativeField(t *testing.T, fixture string) string {
	t.Helper()
	return `"native_catalog_file": "` + filepath.ToSlash(testConfigPath(t, fixture)) + `"`
}

func TestNativeEntriesArePreservedExactly(t *testing.T) {
	bySlug, _ := build(t, `{
	  "listen": {"host": "127.0.0.1", "port": 0},
	  "native": {"chatgpt_base_url": "https://native.invalid/b", "api_base_url": "https://native.invalid/v1", "models": ["gpt-native-top","gpt-native-hidden","gpt-native-second"]},
	  "catalog": {`+nativeField(t, "native-catalog.json")+`},
	  "routes": [{"name": "local", "base_url": "http://127.0.0.1:30000/v1", "models": ["local-model"]}]
	}`)

	original, err := os.ReadFile(testConfigPath(t, "native-catalog.json"))
	if err != nil {
		t.Fatal(err)
	}
	var source struct {
		Models []map[string]json.RawMessage `json:"models"`
	}
	if err := json.Unmarshal(original, &source); err != nil {
		t.Fatal(err)
	}
	if len(source.Models) != 3 {
		t.Fatalf("fixture changed: %d models", len(source.Models))
	}
	for _, want := range source.Models {
		var slug string
		if err := json.Unmarshal(want["slug"], &slug); err != nil {
			t.Fatal(err)
		}
		got, ok := bySlug[slug]
		if !ok {
			t.Fatalf("native entry %q disappeared", slug)
		}
		for key, wantValue := range want {
			gotValue, present := got[key]
			if !present {
				t.Fatalf("native entry %q lost %s", slug, key)
			}
			if compact(wantValue) != compact(gotValue) {
				t.Fatalf("native entry %q field %s changed:\n want %s\n got  %s", slug, key, compact(wantValue), compact(gotValue))
			}
		}
		// A native-only field that this package knows nothing about must survive,
		// which is the point of copying entries rather than rebuilding them.
		if _, ok := got["comp_hash"]; !ok {
			t.Fatalf("native-only field comp_hash was dropped from %q", slug)
		}
	}
}

func TestRootFieldsSurviveTheMerge(t *testing.T) {
	_, data := build(t, `{
	  "listen": {"host": "127.0.0.1", "port": 0},
	  "native": {"chatgpt_base_url": "https://native.invalid/b", "api_base_url": "https://api.invalid/v1", "models": ["gpt-native-top"]},
	  "catalog": {`+nativeField(t, "native-catalog-extra-root.json")+`},
	  "routes": [{"name": "local", "base_url": "http://127.0.0.1:30000/v1", "models": ["local-model"]}]
	}`)
	if !strings.Contains(string(data), `"revision"`) {
		t.Fatalf("the catalog's root fields should survive:\n%s", data)
	}
}

func TestLocalEntriesInheritNativeShapeAndAddPickerFields(t *testing.T) {
	bySlug, data := build(t, `{
	  "listen": {"host": "127.0.0.1", "port": 0},
	  "native": {"chatgpt_base_url": "https://native.invalid/b", "api_base_url": "https://api.invalid/v1", "models": ["gpt-native-top"]},
	  "catalog": {
	    `+nativeField(t, "native-catalog.json")+`,
	    "models": [{
	      "id": "qwen3-32b",
	      "route": "local",
	      "display_name": "Qwen3 32B (local)",
	      "context_window": 131072,
	      "default_reasoning_level": "medium",
	      "tool_capable": true,
	      "input_modalities": ["text"]
	    }]
	  },
	  "routes": [{"name": "local", "reasoning": {"supported_efforts": ["none", "low", "medium", "high"]}, "base_url": "http://127.0.0.1:30000/v1", "models": ["qwen3-32b"]}]
	}`)

	entry, ok := bySlug["qwen3-32b"]
	if !ok {
		t.Fatalf("the self-hosted entry is missing: %s", data)
	}

	native, err := os.ReadFile(testConfigPath(t, "native-catalog.json"))
	if err != nil {
		t.Fatal(err)
	}
	var source struct {
		Models []map[string]json.RawMessage `json:"models"`
	}
	if err := json.Unmarshal(native, &source); err != nil {
		t.Fatal(err)
	}
	// Every field a native entry carries, except the provider-only ones, must be
	// present on the generated entry. Otherwise Codex would read an entry shape it
	// does not expect.
	providerOnly := map[string]bool{
		"comp_hash": true, "service_tiers": true, "additional_speed_tiers": true,
		"availability_nux": true, "upgrade": true, "model_specialty": true,
		"multi_agent_version": true, "tool_mode": true,
		"node_repl_auto_review_required": true, "node_repl_disabled": true,
	}
	for _, key := range sortedKeys(source.Models[0]) {
		if providerOnly[key] {
			continue
		}
		if _, present := entry[key]; !present {
			t.Fatalf("the generated entry is missing %s", key)
		}
	}

	checks := map[string]string{
		"display_name":         "Qwen3 32B (local)",
		"visibility":           "list",
		"shell_type":           "unified_exec",
		"description":          "Self-hosted model",
		"context_window":       "131072",
		"web_search_tool_type": "text",
	}
	for key, want := range checks {
		if got := compact(entry[key]); got != `"`+want+`"` && got != want {
			t.Fatalf("%s = %s, want %s", key, got, want)
		}
	}
	if compact(entry["supported_in_api"]) != "true" {
		t.Fatal("a self-hosted model must stay visible in API-key sessions")
	}
	// The native prompt template must not be inherited: model_messages is cleared so
	// base_instructions is what Codex uses.
	if compact(entry["model_messages"]) != "null" {
		t.Fatalf("model_messages should be null, got %s", compact(entry["model_messages"]))
	}
	var instructions string
	if err := json.Unmarshal(entry["base_instructions"], &instructions); err != nil || !strings.Contains(instructions, "Synthetic native instructions") {
		t.Fatalf("base_instructions should be inherited from the native catalog: %s", compact(entry["base_instructions"]))
	}
	var levels []map[string]string
	if err := json.Unmarshal(entry["supported_reasoning_levels"], &levels); err != nil {
		t.Fatalf("supported_reasoning_levels: %v", err)
	}
	if len(levels) != 4 {
		t.Fatalf("supported_reasoning_levels = %d entries, want 4", len(levels))
	}
	for i, level := range levels {
		if want := []string{"none", "low", "medium", "high"}[i]; level["effort"] != want {
			t.Fatalf("effort[%d] = %q, want %q", i, level["effort"], want)
		}
		if level["description"] == "" || level["effort"] == "" {
			t.Fatalf("reasoning level needs effort and description: %v", level)
		}
	}
	var defaultLevel string
	if err := json.Unmarshal(entry["default_reasoning_level"], &defaultLevel); err != nil || defaultLevel != "medium" {
		t.Fatalf("default_reasoning_level = %s", compact(entry["default_reasoning_level"]))
	}
	if compact(entry["supports_parallel_tool_calls"]) != "true" {
		t.Fatal("tool_capable should set supports_parallel_tool_calls")
	}
	if entry["node_repl_auto_review_required"] != nil {
		t.Fatal("native node_repl state must not be copied onto a self-hosted model")
	}
	if entry["tool_mode"] != nil {
		t.Fatal("native tool_mode state must not be copied onto a self-hosted model")
	}
	// Priority places the self-hosted entry after the native ones, so the bundled
	// models keep their order.
	var priority int
	if err := json.Unmarshal(entry["priority"], &priority); err != nil {
		t.Fatal(err)
	}
	if priority < 8 {
		t.Fatalf("priority = %d, want a value after the native entries", priority)
	}
}

func TestLocalEntryCanOverrideAnyField(t *testing.T) {
	bySlug, _ := build(t, `{
	  "listen": {"host": "127.0.0.1", "port": 0},
	  "native": {"chatgpt_base_url": "https://native.invalid/b", "api_base_url": "https://api.invalid/v1", "models": ["gpt-native-top"]},
	  "catalog": {
	    `+nativeField(t, "native-catalog.json")+`,
	    "models": [{
	      "id": "qwen3-32b", "route": "local",
	      "base_instructions": "Project-specific instructions.",
	      "description": "Lab model",
	      "extra_fields": {"supports_search_tool": true, "custom_native_field": {"a": 1}}
	    }]
	  },
	  "routes": [{"name": "local", "base_url": "http://127.0.0.1:30000/v1", "models": ["qwen3-32b"]}]
	}`)
	entry := bySlug["qwen3-32b"]
	var instructions string
	if err := json.Unmarshal(entry["base_instructions"], &instructions); err != nil || instructions != "Project-specific instructions." {
		t.Fatalf("base_instructions override failed: %s", compact(entry["base_instructions"]))
	}
	if compact(entry["supports_search_tool"]) != "true" {
		t.Fatalf("extra_fields override failed: %s", compact(entry["supports_search_tool"]))
	}
	if compact(entry["custom_native_field"]) != `{"a":1}` {
		t.Fatalf("extra_fields did not add the new key: %s", compact(entry["custom_native_field"]))
	}
	if compact(entry["description"]) != `"Lab model"` {
		t.Fatalf("description override failed: %s", compact(entry["description"]))
	}
}

func TestLocalEntriesWithoutNativeCatalogNeedInstructions(t *testing.T) {
	directory := t.TempDir()
	instructions := filepath.Join(directory, "instructions.txt")
	if err := os.WriteFile(instructions, []byte("You are a coding agent on a lab server.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	bySlug, _ := build(t, `{
	  "listen": {"host": "127.0.0.1", "port": 0},
	  "native": {"chatgpt_base_url": "https://native.invalid/b", "api_base_url": "https://api.invalid/v1", "models": ["gpt-5"]},
	  "catalog": {
	    "base_instructions_file": "`+filepath.ToSlash(instructions)+`",
	    "models": [{"id": "qwen3-32b", "route": "local"}]
	  },
	  "routes": [{"name": "local", "base_url": "http://127.0.0.1:30000/v1", "models": ["qwen3-32b"]}]
	}`)
	var instructionsValue string
	if err := json.Unmarshal(bySlug["qwen3-32b"]["base_instructions"], &instructionsValue); err != nil {
		t.Fatal(err)
	}
	if instructionsValue != "You are a coding agent on a lab server." {
		t.Fatalf("base_instructions = %q", instructionsValue)
	}

	_, err := catalog.Build(mustParse(t, `{
	  "listen": {"host": "127.0.0.1", "port": 0},
	  "native": {"chatgpt_base_url": "https://native.invalid/b", "api_base_url": "https://api.invalid/v1", "models": ["gpt-5"]},
	  "catalog": {"models": [{"id": "qwen3-32b", "route": "local"}]},
	  "routes": [{"name": "local", "base_url": "http://127.0.0.1:30000/v1", "models": ["qwen3-32b"]}]
	}`))
	if err == nil || !strings.Contains(err.Error(), "base_instructions") {
		t.Fatalf("expected a base_instructions error, got %v", err)
	}
}

func TestMinimalNativeCatalogDoesNotForceExtraFields(t *testing.T) {
	bySlug, _ := build(t, `{
	  "listen": {"host": "127.0.0.1", "port": 0},
	  "native": {"chatgpt_base_url": "https://native.invalid/b", "api_base_url": "https://api.invalid/v1", "models": ["plain-model"]},
	  "catalog": {
	    `+nativeField(t, "native-catalog-minimal.json")+`,
	    "base_instructions_file": "",
	    "models": [{"id": "qwen3-32b", "route": "local"}]
	  },
	  "routes": [{"name": "local", "base_url": "http://127.0.0.1:30000/v1", "models": ["qwen3-32b"]}]
	}`)
	if _, ok := bySlug["qwen3-32b"]; !ok {
		t.Fatal("a minimal native catalog should still allow a generated entry")
	}
}

func TestReplacingANativeModelIsRefused(t *testing.T) {
	// Listing the same ID under a route and the native catalog is refused while
	// generating the catalog.
	_, configError := catalog.Build(mustParse(t, `{
	  "listen": {"host": "127.0.0.1", "port": 0},
	  "native": {"chatgpt_base_url": "https://native.invalid/b", "api_base_url": "https://api.invalid/v1", "models": []},
	  "catalog": {`+nativeField(t, "native-catalog.json")+`},
	  "routes": [{"name": "local", "base_url": "http://127.0.0.1:30000/v1", "models": ["gpt-native-top"]}]
	}`))
	if configError == nil || !strings.Contains(configError.Error(), "replaces a native model") {
		t.Fatalf("expected a generation refusal, got %v", configError)
	}

	// With the native slug import switched off, generation still refuses to publish
	// an entry that replaces a model the native catalog already has.
	_, err := catalog.Build(mustParse(t, `{
	  "listen": {"host": "127.0.0.1", "port": 0},
	  "native": {"chatgpt_base_url": "https://native.invalid/b", "api_base_url": "https://api.invalid/v1", "models": []},
	  "catalog": {
	    `+nativeField(t, "native-catalog.json")+`,
	    "native_model_ids_from_catalog": false,
	    "models": [{"id": "gpt-native-top", "route": "local", "base_instructions": "shadow"}]
	  },
	  "routes": [{"name": "local", "base_url": "http://127.0.0.1:30000/v1", "models": ["gpt-native-top"]}]
	}`))
	if err == nil || !strings.Contains(err.Error(), "replaces a native model") {
		t.Fatalf("expected a shadowing refusal, got %v", err)
	}
}

func TestUnknownReasoningLevelIsRefused(t *testing.T) {
	_, err := catalog.Build(mustParse(t, `{
	  "listen": {"host": "127.0.0.1", "port": 0},
	  "native": {"chatgpt_base_url": "https://native.invalid/b", "api_base_url": "https://api.invalid/v1", "models": ["gpt-5"]},
	  "catalog": {"models": [{"id": "m", "route": "local", "base_instructions": "x", "default_reasoning_level": "maximal"}]},
	  "routes": [{"name": "local", "reasoning": {"supported_efforts": ["maximal"]}, "base_url": "http://127.0.0.1:30000/v1", "models": ["m"]}]
	}`))
	if err == nil || !strings.Contains(err.Error(), "unknown reasoning level") {
		t.Fatalf("expected an unknown reasoning level refusal, got %v", err)
	}
}

func TestReasoningLevelsRequireADefault(t *testing.T) {
	_, err := catalog.Build(mustParse(t, `{
	  "listen": {"host": "127.0.0.1", "port": 0},
	  "native": {"chatgpt_base_url": "https://native.invalid/b", "api_base_url": "https://api.invalid/v1", "models": ["gpt-5"]},
	  "catalog": {"models": [{"id": "m", "route": "local", "base_instructions": "x"}]},
	  "routes": [{"name": "local", "reasoning": {"supported_efforts": ["low", "high"]}, "base_url": "http://127.0.0.1:30000/v1", "models": ["m"]}]
	}`))
	if err == nil || !strings.Contains(err.Error(), "default_reasoning_level") {
		t.Fatalf("expected a default-level error, got %v", err)
	}
}

func TestGenerateWritesAtomicallyAndReportsDestination(t *testing.T) {
	directory := t.TempDir()
	output := filepath.Join(directory, "nested", "catalog.json")
	cfg := mustParse(t, `{
	  "listen": {"host": "127.0.0.1", "port": 0},
	  "native": {"chatgpt_base_url": "https://native.invalid/b", "api_base_url": "https://api.invalid/v1", "models": ["gpt-5"]},
	  "catalog": {
	    "output_file": "`+filepath.ToSlash(output)+`",
	    "base_instructions_file": "",
	    "models": [{"id": "m", "route": "local", "base_instructions": "x"}]
	  },
	  "routes": [{"name": "local", "base_url": "http://127.0.0.1:30000/v1", "models": ["m"]}]
	}`)
	data, destination, err := catalog.Generate(cfg, "")
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if destination != output {
		t.Fatalf("destination = %q, want %q", destination, output)
	}
	onDisk, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if string(onDisk) != string(data) {
		t.Fatal("the file does not match the returned catalog")
	}
	entries, err := catalog.CountModels(data)
	if err != nil {
		t.Fatal(err)
	}
	if entries != 1 {
		t.Fatalf("entries = %d", entries)
	}
	// No leftover temporary files in the output directory.
	remaining, err := os.ReadDir(filepath.Dir(output))
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 1 {
		t.Fatalf("temporary files were left behind: %v", remaining)
	}
	// An explicit --out wins over the configured path.
	other := filepath.Join(directory, "other.json")
	if _, destination, err := catalog.Generate(cfg, other); err != nil || destination != other {
		t.Fatalf("explicit out: %q %v", destination, err)
	}
}

func TestGenerateWithoutAnOutputPathReturnsBytesOnly(t *testing.T) {
	cfg := mustParse(t, `{
	  "listen": {"host": "127.0.0.1", "port": 0},
	  "native": {"chatgpt_base_url": "https://native.invalid/b", "api_base_url": "https://api.invalid/v1", "models": ["gpt-5"]},
	  "catalog": {"models": [{"id": "m", "route": "local", "base_instructions": "x"}]},
	  "routes": [{"name": "local", "base_url": "http://127.0.0.1:30000/v1", "models": ["m"]}]
	}`)
	data, destination, err := catalog.Generate(cfg, "")
	if err != nil {
		t.Fatal(err)
	}
	if destination != "" {
		t.Fatalf("destination = %q, want none", destination)
	}
	if len(data) == 0 {
		t.Fatal("no catalog bytes returned")
	}
}

func TestNativeSlugsAreReported(t *testing.T) {
	data, err := os.ReadFile(testConfigPath(t, "native-catalog.json"))
	if err != nil {
		t.Fatal(err)
	}
	slugs, err := catalog.NativeSlugs(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(slugs) != 3 || slugs[0] != "gpt-native-top" {
		t.Fatalf("slugs = %v", slugs)
	}
	if _, err := catalog.NativeSlugs([]byte(`{"models":[{"display_name":"no slug"}]}`)); err == nil {
		t.Fatal("an entry without a slug should be an error")
	}
}

// compact renders a JSON value for comparison. A missing key is reported as the
// distinct marker <missing> so a test cannot confuse it with an explicit null.
func compact(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "<missing>"
	}
	buffer := &bytes.Buffer{}
	if err := json.Compact(buffer, raw); err != nil {
		return string(raw)
	}
	return buffer.String()
}

func sortedKeys(entry map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(entry))
	for key := range entry {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

func mustParse(t *testing.T, text string) *config.Config {
	t.Helper()
	cfg, err := config.Parse([]byte(text))
	if err != nil {
		t.Fatalf("config: %v\n%s", err, text)
	}
	return cfg
}

func TestExtraFieldsCannotReplaceEntryIdentity(t *testing.T) {
	// extra_fields is for keys this program does not model. Overwriting the slug
	// would publish a self-hosted entry under a native model's identity, past every
	// collision check that was made on the configured id.
	for _, key := range []string{"slug", "visibility", "shell_type", "priority", "base_instructions", "model_messages", "supported_reasoning_levels", "default_reasoning_level", "supported_in_api"} {
		_, err := catalog.Build(mustParse(t, `{
	  "listen": {"host": "127.0.0.1", "port": 0},
	  "native": {"chatgpt_base_url": "https://native.invalid/b", "api_base_url": "https://api.invalid/v1", "models": []},
	  "catalog": {
	    `+nativeField(t, "native-catalog.json")+`,
	    "models": [{"id": "qwen3-32b", "route": "local",
	      "extra_fields": {"`+key+`": "whatever"}}]
	  },
	  "routes": [{"name": "local", "base_url": "http://127.0.0.1:30000/v1", "models": ["qwen3-32b"]}]
	}`))
		if err == nil || !strings.Contains(err.Error(), "the router controls") {
			t.Fatalf("extra_fields.%s was accepted: %v", key, err)
		}
	}

	// A key nobody modelled is still allowed, which is the point of the field.
	bySlug, _ := build(t, `{
	  "listen": {"host": "127.0.0.1", "port": 0},
	  "native": {"chatgpt_base_url": "https://native.invalid/b", "api_base_url": "https://api.invalid/v1", "models": []},
	  "catalog": {
	    `+nativeField(t, "native-catalog.json")+`,
	    "models": [{"id": "qwen3-32b", "route": "local", "extra_fields": {"some_future_key": 3}}]
	  },
	  "routes": [{"name": "local", "base_url": "http://127.0.0.1:30000/v1", "models": ["qwen3-32b"]}]
	}`)
	if _, ok := bySlug["qwen3-32b"]; !ok {
		t.Fatal("an unmodelled extra field should still be accepted")
	}
}

// TestDisplayNameIsIndependentFromRoutingSlug covers a catalog entry whose picker label
// and routing identity share almost nothing: the ID carries a vendor namespace and a
// quantisation suffix, the label is human-readable prose. The slug is what the router
// matches requests against, so it has to survive verbatim, and the label must not be
// allowed to influence it.
func TestDisplayNameIsIndependentFromRoutingSlug(t *testing.T) {
	const (
		routingID   = "nvidia/Qwen3.8-Flash-Next-NVFP4"
		pickerLabel = "Qwen-3.8 Flash Next"
	)
	bySlug, _ := build(t, `{
	  "listen": {"host": "127.0.0.1", "port": 0},
	  "native": {"chatgpt_base_url": "https://native.invalid/b", "api_base_url": "https://native.invalid/v1", "models": []},
	  "catalog": {`+nativeField(t, "native-catalog.json")+`,
	    "models": [{
	      "id": "`+routingID+`",
	      "route": "sglang",
	      "display_name": "`+pickerLabel+`",
	      "default_reasoning_level": "none"
	    }]
	  },
	  "routes": [{"name": "sglang", "reasoning": {"supported_efforts": ["none", "low", "high"]}, "base_url": "http://127.0.0.1:4011/v1", "models": ["`+routingID+`"]}]
	}`)

	entry, ok := bySlug[routingID]
	if !ok {
		t.Fatalf("the entry was not published under its exact id; slugs: %v", keysOf(bySlug))
	}
	var display string
	if err := json.Unmarshal(entry["display_name"], &display); err != nil {
		t.Fatalf("display_name: %v", err)
	}
	if display != pickerLabel {
		t.Fatalf("display_name = %q, want %q", display, pickerLabel)
	}
	// The routing metadata is derived from the ID and the route, never from the label:
	// a namespace and a suffix that appear only in the ID must still be in the slug.
	if !strings.Contains(routingID, "/") || strings.Contains(pickerLabel, "/") {
		t.Fatal("the fixtures must differ in shape for this test to mean anything")
	}
	if string(entry["slug"]) != `"`+routingID+`"` {
		t.Fatalf("slug = %s, want the id verbatim", entry["slug"])
	}

	// Identity is byte-exact, which is what makes the picker label safe to write freely:
	// an ID that differs only in case is a different model, not a rename.
	twoLabels, _ := build(t, `{
	  "listen": {"host": "127.0.0.1", "port": 0},
	  "native": {"chatgpt_base_url": "https://native.invalid/b", "api_base_url": "https://native.invalid/v1", "models": []},
	  "catalog": {`+nativeField(t, "native-catalog.json")+`,
	    "models": [
	      {"id": "`+routingID+`", "route": "sglang", "display_name": "`+pickerLabel+`", "base_instructions": "a"},
	      {"id": "NVIDIA/Qwen3.8-Flash-Next-NVFP4", "route": "sglang", "display_name": "Other vendor build", "base_instructions": "b"}
	    ]
	  },
	  "routes": [{"name": "sglang", "base_url": "http://127.0.0.1:4011/v1", "models": ["`+routingID+`","NVIDIA/Qwen3.8-Flash-Next-NVFP4"]}]
	}`)
	if _, ok := twoLabels["NVIDIA/Qwen3.8-Flash-Next-NVFP4"]; !ok {
		t.Fatalf("the case variant was folded into the first entry: %v", keysOf(twoLabels))
	}

	// A second label cannot be smuggled in beside the same slug, either: the ID decides
	// identity, so a tag-suffixed repeat of it is refused rather than published twice.
	_, err := catalog.Build(mustParse(t, `{
	  "listen": {"host": "127.0.0.1", "port": 0},
	  "native": {"chatgpt_base_url": "https://native.invalid/b", "api_base_url": "https://native.invalid/v1", "models": []},
	  "catalog": {`+nativeField(t, "native-catalog.json")+`,
	    "models": [
	      {"id": "`+routingID+`", "route": "sglang", "display_name": "`+pickerLabel+`", "base_instructions": "a"},
	      {"id": "`+routingID+`:latest", "route": "sglang", "display_name": "Another label", "base_instructions": "b"}
	    ]
	  },
	  "routes": [{"name": "sglang", "base_url": "http://127.0.0.1:4011/v1", "models": ["`+routingID+`","`+routingID+`:latest"]}]
	}`))
	// Either collision check may be the one that fires; both mean the same thing here.
	if err == nil || !(strings.Contains(err.Error(), "duplicate id") || strings.Contains(err.Error(), "already taken")) {
		t.Fatalf("two ids that resolve to one slug were accepted: %v", err)
	}

	// The reverse also holds: distinct IDs may share a label, because the label is not
	// an identity and is never used to route.
	shared, _ := build(t, `{
	  "listen": {"host": "127.0.0.1", "port": 0},
	  "native": {"chatgpt_base_url": "https://native.invalid/b", "api_base_url": "https://native.invalid/v1", "models": []},
	  "catalog": {`+nativeField(t, "native-catalog.json")+`,
	    "models": [
	      {"id": "nvidia/Qwen3.8-Flash-Next-NVFP4", "route": "sglang", "display_name": "`+pickerLabel+`", "base_instructions": "a"},
	      {"id": "nvidia/Qwen3.8-Flash-Next", "route": "sglang", "display_name": "`+pickerLabel+`", "base_instructions": "b"}
	    ]
	  },
	  "routes": [{"name": "sglang", "base_url": "http://127.0.0.1:4011/v1", "models": ["nvidia/Qwen3.8-Flash-Next-NVFP4","nvidia/Qwen3.8-Flash-Next"]}]
	}`)
	for _, id := range []string{"nvidia/Qwen3.8-Flash-Next-NVFP4", "nvidia/Qwen3.8-Flash-Next"} {
		if _, ok := shared[id]; !ok {
			t.Fatalf("%s is missing from %v", id, keysOf(shared))
		}
	}
}

func keysOf(bySlug map[string]map[string]json.RawMessage) []string {
	names := make([]string, 0, len(bySlug))
	for slug := range bySlug {
		names = append(names, slug)
	}
	slices.Sort(names)
	return names
}

// TestUnreadableBaseInstructionsFileStopsGeneration covers the case where the operator
// points at an instructions file that cannot be read while the native template happens
// to carry instructions of its own. Silently using the template would send every local
// model a set of instructions nobody chose, and nothing in the output would say so.
func TestUnreadableBaseInstructionsFileStopsGeneration(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent", "instructions.md")
	templateInstructions := "Synthetic native instructions for gpt-native"

	_, err := catalog.Build(mustParse(t, `{
	  "listen": {"host": "127.0.0.1", "port": 0},
	  "native": {"chatgpt_base_url": "https://native.invalid/b", "api_base_url": "https://native.invalid/v1", "models": []},
	  "catalog": {`+nativeField(t, "native-catalog.json")+`,
	    "base_instructions_file": "`+filepath.ToSlash(missing)+`",
	    "models": [{"id": "qwen3-32b", "route": "local", "base_instructions": "explicit inline instructions"}]
	  },
	  "routes": [{"name": "local", "base_url": "http://127.0.0.1:30000/v1", "models": ["qwen3-32b"]}]
	}`))
	if err == nil {
		t.Fatalf("an unreadable instructions file was accepted; the native template %q would have been substituted", templateInstructions)
	}
	if !strings.Contains(err.Error(), "base_instructions_file") || !strings.Contains(err.Error(), missing) {
		t.Fatalf("the error must name the configured path, got: %v", err)
	}

	// A directory is the same class of failure: the path exists and still cannot be
	// read as instructions.
	_, err = catalog.Build(mustParse(t, `{
	  "listen": {"host": "127.0.0.1", "port": 0},
	  "native": {"chatgpt_base_url": "https://native.invalid/b", "api_base_url": "https://native.invalid/v1", "models": []},
	  "catalog": {`+nativeField(t, "native-catalog.json")+`,
	    "base_instructions_file": "`+filepath.ToSlash(t.TempDir())+`",
	    "models": [{"id": "qwen3-32b", "route": "local"}]
	  },
	  "routes": [{"name": "local", "base_url": "http://127.0.0.1:30000/v1", "models": ["qwen3-32b"]}]
	}`))
	if err == nil || !strings.Contains(err.Error(), "base_instructions_file") {
		t.Fatalf("a directory as the instructions file should stop generation: %v", err)
	}

	// When the file is readable it wins over the template, which is the whole reason it
	// is configured, and a readable file is never treated as a failure.
	directory := t.TempDir()
	path := filepath.Join(directory, "instructions.md")
	if err := os.WriteFile(path, []byte("  Instructions from the configured file.  \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	bySlug, _ := build(t, `{
	  "listen": {"host": "127.0.0.1", "port": 0},
	  "native": {"chatgpt_base_url": "https://native.invalid/b", "api_base_url": "https://native.invalid/v1", "models": []},
	  "catalog": {`+nativeField(t, "native-catalog.json")+`,
	    "base_instructions_file": "`+filepath.ToSlash(path)+`",
	    "models": [{"id": "qwen3-32b", "route": "local"}]
	  },
	  "routes": [{"name": "local", "base_url": "http://127.0.0.1:30000/v1", "models": ["qwen3-32b"]}]
	}`)
	entry, ok := bySlug["qwen3-32b"]
	if !ok {
		t.Fatalf("the entry is missing from %v", keysOf(bySlug))
	}
	var instructions string
	if err := json.Unmarshal(entry["base_instructions"], &instructions); err != nil {
		t.Fatalf("base_instructions: %v", err)
	}
	if instructions != "Instructions from the configured file." {
		t.Fatalf("base_instructions = %q, want the configured file's content, trimmed", instructions)
	}
}

func TestLegacyCatalogReasoningLevelsAreRejected(t *testing.T) {
	_, err := config.Parse([]byte(`{"catalog":{"models":[{"id":"m","route":"local","reasoning_levels":["low"]}]}}`))
	if err == nil || !strings.Contains(err.Error(), `unknown field "reasoning_levels"`) {
		t.Fatalf("expected removed field to be rejected, got %v", err)
	}
}

func TestDefaultMustBelongToRouteEfforts(t *testing.T) {
	_, err := config.Parse([]byte(`{
 "routes":[{"name":"local","base_url":"http://127.0.0.1:4011/v1","models":["m"],"reasoning":{"supported_efforts":["none","low","medium","xhigh"]}}],
 "catalog":{"models":[{"id":"m","route":"local","default_reasoning_level":"high"}]}
 }`))
	if err == nil || !strings.Contains(err.Error(), "not in route reasoning.supported_efforts") {
		t.Fatalf("expected route/default mismatch to be rejected, got %v", err)
	}
}
