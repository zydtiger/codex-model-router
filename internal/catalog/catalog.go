// Package catalog builds the combined Codex model catalog that puts native and
// self-hosted models in the same picker.
package catalog

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/zydtiger/codex-model-router/internal/config"
)

// Filename is the conventional name for the generated catalog next to the Codex
// config file.
const Filename = "codex-model-router-catalog.json"

// EffortDescriptions are the picker descriptions for reasoning levels. Codex
// requires a description per effort.
var EffortDescriptions = map[string]string{
	"none":    "Turn thinking off",
	"minimal": "Quick answers with minimal thinking",
	"low":     "Fast responses with lighter thinking",
	"medium":  "Balances speed and thinking depth for everyday tasks",
	"high":    "Greater thinking depth for complex tasks",
	"xhigh":   "Extra high thinking depth for complex problems",
	"max":     "Maximum thinking depth for the hardest problems",
	"ultra":   "Maximum thinking depth with automatic task delegation",
}

// knownEfforts is the effort vocabulary Codex accepts.
var knownEfforts = []string{"none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra"}

// rawCatalog keeps unknown native fields intact.
type rawCatalog struct {
	Models []json.RawMessage `json:"models"`

	extra map[string]json.RawMessage
}

func (c *rawCatalog) UnmarshalJSON(data []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	models, ok := fields["models"]
	if !ok {
		return errors.New("model catalog has no models field")
	}
	if err := json.Unmarshal(models, &c.Models); err != nil {
		return fmt.Errorf("model catalog models field: %w", err)
	}
	delete(fields, "models")
	c.extra = fields
	return nil
}

func (c rawCatalog) MarshalJSON() ([]byte, error) {
	fields := map[string]json.RawMessage{}
	for name, value := range c.extra {
		fields[name] = value
	}
	models, err := json.Marshal(c.Models)
	if err != nil {
		return nil, err
	}
	fields["models"] = models
	return json.Marshal(fields)
}

// Build merges a native catalog with configured local entries.
//
// Native entries are copied field for field, including any key this package has
// never heard of, so a Codex upgrade does not quietly lose metadata. Local
// entries are generated from the shape of a native entry and then checked for
// the fields a native entry always carries.
func Build(cfg *config.Config) ([]byte, error) {
	nativeData, err := readNativeCatalog(cfg)
	if err != nil {
		return nil, err
	}

	native := rawCatalog{}
	nativeEntries := []json.RawMessage{}
	nativeSlugs := map[string]bool{}
	template := map[string]json.RawMessage{}
	if len(bytes.TrimSpace(nativeData)) > 0 {
		if err := json.Unmarshal(nativeData, &native); err != nil {
			return nil, fmt.Errorf("native catalog: %w", err)
		}
		nativeEntries = native.Models
		if len(nativeEntries) == 0 {
			return nil, errors.New("native catalog contains no models")
		}
		for index, entry := range nativeEntries {
			slug, fields, err := entryFields(entry)
			if err != nil {
				return nil, fmt.Errorf("native catalog models[%d]: %w", index, err)
			}
			key := modelKey(slug)
			if nativeSlugs[key] {
				return nil, fmt.Errorf("native catalog models[%d]: duplicate slug %q", index, slug)
			}
			nativeSlugs[key] = true
			if len(template) == 0 && usableTemplate(fields) {
				template = fields
			}
		}
	}

	baseInstructions, err := resolveBaseInstructions(cfg, template)
	if err != nil {
		return nil, err
	}
	defaultDescription := strings.TrimSpace(cfg.Catalog.Description)
	if defaultDescription == "" {
		defaultDescription = "Self-hosted model"
	}

	localEntries := make([]json.RawMessage, 0, len(cfg.Catalog.Models))
	seen := map[string]bool{}
	required := requiredTemplateKeys(template)
	priority := nextPriority(nativeEntries)
	for _, model := range cfg.Catalog.Models {
		key := modelKey(model.ID)
		if nativeSlugs[key] {
			return nil, fmt.Errorf("catalog.models: %q replaces a native model; rename it or remove it from native.models", model.ID)
		}
		if seen[key] {
			return nil, fmt.Errorf("catalog.models: duplicate id %q", model.ID)
		}

		entry, err := localEntry(model, template, baseInstructions, defaultDescription, priority)
		if err != nil {
			return nil, err
		}
		// The checks above use the configured ID. The entry is verified by the slug it
		// actually carries, so no step in between can produce a duplicate.
		finalSlug, err := entrySlug(entry)
		if err != nil {
			return nil, fmt.Errorf("catalog.models: %q: %w", model.ID, err)
		}
		if nativeSlugs[finalSlug] || seen[finalSlug] {
			return nil, fmt.Errorf("catalog.models: entry %q would be published as %q, which is already taken", model.ID, finalSlug)
		}
		seen[key] = true
		seen[finalSlug] = true
		if missing := missingKeys(entry, required); len(missing) > 0 {
			return nil, fmt.Errorf("catalog.models: entry %q is missing %s; the native catalog shape requires these fields",
				model.ID, strings.Join(missing, ", "))
		}
		encoded, err := json.Marshal(entry)
		if err != nil {
			return nil, err
		}
		localEntries = append(localEntries, encoded)
		priority++
	}

	merged := rawCatalog{Models: append(append([]json.RawMessage{}, nativeEntries...), localEntries...), extra: native.extra}
	if len(merged.extra) == 0 {
		merged.extra = map[string]json.RawMessage{}
	}
	encoded, err := json.MarshalIndent(merged, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(encoded, '\n'), nil
}

// Generate builds the catalog and writes it to the configured output path, or to
// an explicit path. Nothing is written when writing fails partway: the file is
// replaced atomically.
func Generate(cfg *config.Config, outputPath string) ([]byte, string, error) {
	encoded, err := Build(cfg)
	if err != nil {
		return nil, "", err
	}
	destination := strings.TrimSpace(outputPath)
	if destination == "" {
		destination = strings.TrimSpace(cfg.Catalog.OutputFile)
	}
	if destination == "" {
		return encoded, "", nil
	}
	if err := WriteAtomic(destination, encoded); err != nil {
		return nil, "", err
	}
	return encoded, destination, nil
}

// WriteAtomic replaces a file through a temporary file in the same directory.
func WriteAtomic(path string, data []byte) error {
	if strings.TrimSpace(path) == "" {
		return errors.New("output path is empty")
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return err
	}
	temp, err := os.CreateTemp(directory, ".codex-model-router-*")
	if err != nil {
		return err
	}
	tempName := temp.Name()
	defer func() {
		_ = temp.Close()
		_ = os.Remove(tempName)
	}()
	if _, err := temp.Write(data); err != nil {
		return err
	}
	if err := temp.Chmod(0o644); err != nil {
		return err
	}
	if err := temp.Sync(); err != nil {
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(tempName, path)
}

func readNativeCatalog(cfg *config.Config) ([]byte, error) {
	path := strings.TrimSpace(cfg.Catalog.NativeCatalogFile)
	if path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read native catalog %s: %w", path, err)
	}
	return data, nil
}

func entryFields(entry json.RawMessage) (string, map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(entry, &fields); err != nil {
		return "", nil, fmt.Errorf("entry is not a JSON object: %w", err)
	}
	var slug string
	if raw, ok := fields["slug"]; !ok {
		return "", nil, errors.New("entry has no slug")
	} else if err := json.Unmarshal(raw, &slug); err != nil {
		return "", nil, fmt.Errorf("entry slug is not a string: %w", err)
	}
	slug = strings.TrimSpace(slug)
	if slug == "" {
		return "", nil, errors.New("entry has an empty slug")
	}
	return slug, fields, nil
}

// usableTemplate reports whether an entry carries the fields a local entry needs
// to inherit. Hidden entries are still valid shapes.
func usableTemplate(fields map[string]json.RawMessage) bool {
	_, hasShell := fields["shell_type"]
	_, hasTruncation := fields["truncation_policy"]
	return hasShell && hasTruncation
}

// requiredTemplateKeys is the field set a local entry must carry because every
// native entry carries it. Provider-only fields are excluded: they describe
// native infrastructure and cannot be filled in for a self-hosted model.
var providerOnlyKeys = map[string]bool{
	"comp_hash":                      true,
	"service_tiers":                  true,
	"additional_speed_tiers":         true,
	"availability_nux":               true,
	"upgrade":                        true,
	"model_specialty":                true,
	"multi_agent_version":            true,
	"tool_mode":                      true,
	"node_repl_auto_review_required": true,
	"node_repl_disabled":             true,
}

func requiredTemplateKeys(template map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(template))
	for key := range template {
		if providerOnlyKeys[key] {
			continue
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func missingKeys(entry map[string]json.RawMessage, required []string) []string {
	var missing []string
	for _, key := range required {
		if _, ok := entry[key]; !ok {
			missing = append(missing, key)
		}
	}
	return missing
}

func nextPriority(entries []json.RawMessage) int {
	highest := 0
	for _, entry := range entries {
		var model struct {
			Priority int `json:"priority"`
		}
		if json.Unmarshal(entry, &model) == nil && model.Priority > highest {
			highest = model.Priority
		}
	}
	return highest + 1
}

// resolveBaseInstructions returns the instructions a local entry is built on.
//
// A configured file is an explicit statement about what the self-hosted model should be
// told, so a path that cannot be read stops generation. Falling back to the native
// template in that case would send the model a different set of instructions than the
// one the operator pointed at, and nothing would say so.
func resolveBaseInstructions(cfg *config.Config, template map[string]json.RawMessage) (string, error) {
	if path := strings.TrimSpace(cfg.Catalog.BaseInstructionsFile); path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("catalog.base_instructions_file: read %s: %w", path, err)
		}
		return strings.TrimSpace(string(data)), nil
	}
	if raw, ok := template["base_instructions"]; ok {
		var value string
		if json.Unmarshal(raw, &value) == nil {
			return value, nil
		}
	}
	return "", nil
}

// localEntry builds one picker entry.
//
// The entry starts from a native entry so that any field Codex expects but this
// package does not model is still present. Fields that describe the model itself
// are then replaced, and provider-only fields are dropped.
func localEntry(model config.CatalogModel, template map[string]json.RawMessage, baseInstructions, defaultDescription string, priority int) (map[string]json.RawMessage, error) {
	entry := map[string]json.RawMessage{}
	for key, value := range template {
		if providerOnlyKeys[key] {
			continue
		}
		entry[key] = value
	}

	displayName := strings.TrimSpace(model.DisplayName)
	if displayName == "" {
		displayName = model.ID
	}
	description := strings.TrimSpace(model.Description)
	if description == "" {
		description = defaultDescription
	}
	contextWindow := model.ContextWindow
	if contextWindow <= 0 {
		contextWindow = defaultContextWindow
	}
	modalities := model.InputModalities
	if len(modalities) == 0 {
		modalities = []string{"text"}
	}
	instructions := model.BaseInstructions
	if strings.TrimSpace(instructions) == "" {
		instructions = baseInstructions
	}
	if strings.TrimSpace(instructions) == "" {
		return nil, fmt.Errorf("catalog.models: %q has no base_instructions and the native catalog provided none", model.ID)
	}
	levels := model.ReasoningLevels
	for _, level := range levels {
		if !isKnownEffort(level) {
			return nil, fmt.Errorf("catalog.models: %q advertises unknown reasoning level %q", model.ID, level)
		}
	}
	if len(levels) > 0 && model.DefaultReasoningLevel == "" {
		return nil, fmt.Errorf("catalog.models: %q advertises reasoning levels but no default_reasoning_level", model.ID)
	}
	entryPriority := model.Priority
	if entryPriority <= 0 {
		entryPriority = priority
	}

	var defaultLevel any
	if model.DefaultReasoningLevel != "" {
		defaultLevel = model.DefaultReasoningLevel
	}
	supportedLevels := make([]map[string]string, 0, len(levels))
	for _, level := range levels {
		supportedLevels = append(supportedLevels, map[string]string{
			"effort":      level,
			"description": effortDescription(level),
		})
	}

	set := func(key string, value any) error {
		encoded, err := json.Marshal(value)
		if err != nil {
			return fmt.Errorf("catalog.models: %q field %s: %w", model.ID, key, err)
		}
		entry[key] = encoded
		return nil
	}

	for _, field := range []struct {
		key   string
		value any
	}{
		{"slug", model.ID},
		{"display_name", displayName},
		{"description", description},
		{"default_reasoning_level", defaultLevel},
		{"supported_reasoning_levels", supportedLevels},
		{"shell_type", "unified_exec"},
		{"visibility", "list"},
		// A self-hosted model works in both session types, so it stays visible
		// when Codex runs with an API key instead of a ChatGPT account.
		{"supported_in_api", true},
		{"priority", entryPriority},
		{"additional_speed_tiers", []string{}},
		{"service_tiers", []any{}},
		{"default_service_tier", nil},
		{"availability_nux", nil},
		{"upgrade", nil},
		// model_messages would override base_instructions with the native
		// prompt template, so it is cleared deliberately.
		{"model_messages", nil},
		{"base_instructions", instructions},
		{"include_skills_usage_instructions", false},
		{"include_plugin_usage_instructions", false},
		{"include_apps_usage_instructions", false},
		{"supports_reasoning_summary_parameter", false},
		{"supports_reasoning_summaries", len(levels) > 0 && model.DefaultReasoningLevel != "none"},
		{"default_reasoning_summary", "none"},
		{"support_verbosity", false},
		{"default_verbosity", nil},
		{"apply_patch_tool_type", nil},
		{"web_search_tool_type", "text"},
		{"truncation_policy", map[string]any{"mode": "tokens", "limit": 10000}},
		{"supports_parallel_tool_calls", model.ToolCapable},
		{"supports_search_tool", false},
		{"supports_image_detail_original", false},
		{"context_window", contextWindow},
		{"max_context_window", contextWindow},
		{"auto_compact_token_limit", nil},
		{"effective_context_window_percent", 95},
		{"experimental_supported_tools", []string{}},
		{"input_modalities", modalities},
		{"use_responses_lite", false},
	} {
		if err := set(field.key, field.value); err != nil {
			return nil, err
		}
	}

	// extra_fields exists for keys this program does not model. It cannot replace a
	// key the generator controls, because those give the entry its identity and
	// decide which upstream its requests reach; an override there would also defeat
	// the collision checks made on model.ID.
	for key, value := range model.ExperimentalExtra {
		if reservedEntryKey(key) {
			return nil, fmt.Errorf("catalog.models: %q sets %q, which the router controls; use the field provided for it", model.ID, key)
		}
		if err := set(key, value); err != nil {
			return nil, err
		}
	}
	return entry, nil
}

// reservedEntryKey reports whether a catalog key is owned by the generator.
func reservedEntryKey(key string) bool {
	switch key {
	case "slug", "base_instructions", "model_messages", "supported_reasoning_levels",
		"default_reasoning_level", "default_service_tier", "service_tier_label",
		"visibility", "supported_in_api", "shell_type", "priority":
		return true
	}
	return false
}

// entrySlug reads the slug a finished entry will be published under.
func entrySlug(entry map[string]json.RawMessage) (string, error) {
	raw, ok := entry["slug"]
	if !ok {
		return "", errors.New("the generated entry has no slug")
	}
	var slug string
	if err := json.Unmarshal(raw, &slug); err != nil {
		return "", fmt.Errorf("the generated entry's slug is not a string: %w", err)
	}
	return modelKey(slug), nil
}

const defaultContextWindow = 128_000

func isKnownEffort(level string) bool {
	for _, known := range knownEfforts {
		if known == strings.TrimSpace(level) {
			return true
		}
	}
	return false
}

func effortDescription(level string) string {
	if description, ok := EffortDescriptions[strings.TrimSpace(level)]; ok {
		return description
	}
	return "Thinking effort"
}

// modelKey normalizes the ":latest" suffix Codex-style tags may carry.
func modelKey(model string) string {
	return strings.TrimSuffix(strings.TrimSpace(model), ":latest")
}

// NativeSlugs returns the model IDs in a raw native catalog.
func NativeSlugs(data []byte) ([]string, error) {
	catalog := rawCatalog{}
	if err := json.Unmarshal(data, &catalog); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(catalog.Models))
	for index, entry := range catalog.Models {
		slug, _, err := entryFields(entry)
		if err != nil {
			return nil, fmt.Errorf("models[%d]: %w", index, err)
		}
		ids = append(ids, slug)
	}
	return ids, nil
}

// CountModels reports how many entries a generated catalog carries.
func CountModels(data []byte) (int, error) {
	parsed := rawCatalog{}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return 0, err
	}
	return len(parsed.Models), nil
}
