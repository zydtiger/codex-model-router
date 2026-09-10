package config

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// catalogSlugs reads the model IDs from a raw Codex model catalog file. Only
// the slug field is decoded; the rest of the catalog is handled by the catalog
// package when it generates the combined file.
func catalogSlugs(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var catalog struct {
		Models []struct {
			Slug string `json:"slug"`
		} `json:"models"`
	}
	if err := json.Unmarshal(data, &catalog); err != nil {
		return nil, fmt.Errorf("decode %s: %w", path, err)
	}
	if len(catalog.Models) == 0 {
		return nil, fmt.Errorf("%s contains no models", path)
	}
	ids := make([]string, 0, len(catalog.Models))
	for i, model := range catalog.Models {
		slug := strings.TrimSpace(model.Slug)
		if slug == "" {
			return nil, fmt.Errorf("%s: models[%d] has no slug", path, i)
		}
		ids = append(ids, slug)
	}
	return ids, nil
}

// validateEffortMaps checks the adapter's per-effort configuration shape. A map
// value is an effort map: its keys are effort names and its values are the
// literal values sent for that effort. Anything else is a literal kwarg.
func validateEffortMaps(kwargs map[string]json.RawMessage) error {
	for name, raw := range kwargs {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("keyword names must not be empty")
		}
		var effortMap map[string]json.RawMessage
		if err := json.Unmarshal(raw, &effortMap); err != nil {
			continue // literal value: any JSON scalar, bool, or null is fine
		}
		if len(effortMap) == 0 {
			return fmt.Errorf("%q is an empty effort map", name)
		}
		for effort, value := range effortMap {
			if strings.TrimSpace(effort) == "" {
				return fmt.Errorf("%q has an empty effort key", name)
			}
			if strings.ContainsAny(effort, " \t\r\n") {
				return fmt.Errorf("%q has effort key %q with whitespace", name, effort)
			}
			trimmed := strings.TrimSpace(string(value))
			if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
				return fmt.Errorf("%q[%q] must be a scalar value, not %s", name, effort, trimmed)
			}
		}
	}
	return nil
}
