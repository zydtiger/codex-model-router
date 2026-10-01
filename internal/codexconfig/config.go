// Package codexconfig makes the two router-specific edits to a Codex TOML
// configuration without reformatting or discarding unrelated settings.
package codexconfig

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/pelletier/go-toml/v2"
	"github.com/pelletier/go-toml/v2/unstable"
)

// DefaultPath resolves Codex's normal configuration path. A relative
// CODEX_HOME is ignored because it would make the service and the interactive
// CLI use different configuration roots depending on their working directory.
func DefaultPath() (string, error) {
	root := strings.TrimSpace(os.Getenv("CODEX_HOME"))
	if !filepath.IsAbs(root) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		root = filepath.Join(home, ".codex")
	}
	return filepath.Join(root, "config.toml"), nil
}

// Plan describes the content change required for a Codex configuration.
type Plan struct {
	Path           string
	Backup         string
	Original       []byte
	OriginalExists bool
	Updated        []byte
	Changed        bool
}

// Prepare validates existing TOML and prepares narrow root-level edits for the
// router's URL and final catalog. It retains every byte outside those values.
// A root model_provider, including a table or dotted form, is rejected because
// it can route around openai_base_url.
func Prepare(path, baseURL, catalogPath string) (Plan, error) {
	if strings.TrimSpace(baseURL) == "" || strings.TrimSpace(catalogPath) == "" {
		return Plan{}, errors.New("Codex base URL and catalog path are required")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return Plan{}, err
	}
	plan := Plan{Path: absolute, Backup: absolute + ".codex-model-router.bak"}
	original, err := os.ReadFile(absolute)
	if errors.Is(err, os.ErrNotExist) {
		plan.Updated = []byte("openai_base_url = " + strconv.Quote(baseURL) + "\nmodel_catalog_json = " + strconv.Quote(catalogPath) + "\n")
		plan.Changed = true
		return plan, nil
	}
	if err != nil {
		return Plan{}, fmt.Errorf("read Codex configuration %s: %w", absolute, err)
	}
	plan.Original = original
	plan.OriginalExists = true
	if err := validateTOML(original); err != nil {
		return Plan{}, fmt.Errorf("Codex configuration %s: %w", absolute, err)
	}

	edits, insertAt, hasProvider, err := rootEdits(original, baseURL, catalogPath)
	if err != nil {
		return Plan{}, fmt.Errorf("Codex configuration %s: %w", absolute, err)
	}
	if hasProvider {
		return Plan{}, errors.New("root model_provider is configured; remove or migrate that override before configuring the router so Codex cannot bypass openai_base_url")
	}
	updated := applyEdits(original, edits, insertAt)
	if err := validateTOML(updated); err != nil {
		return Plan{}, fmt.Errorf("generated Codex configuration is invalid: %w", err)
	}
	plan.Updated = updated
	plan.Changed = string(updated) != string(original)
	return plan, nil
}

// Apply writes the exact original bytes as a one-time backup before atomically
// replacing the configuration. Existing backup paths are never overwritten.
func (p Plan) Apply() error {
	if !p.Changed {
		return nil
	}
	if err := p.checkCurrent(); err != nil {
		return err
	}
	if p.OriginalExists {
		if info, err := os.Lstat(p.Backup); err == nil {
			// This is a later update. Preserve the exact backup created before the
			// first router edit instead of replacing it with newer bytes.
			if !info.Mode().IsRegular() {
				return fmt.Errorf("existing backup %s is not a regular file", p.Backup)
			}
		} else if errors.Is(err, os.ErrNotExist) {
			if err := writeNewFile(p.Backup, p.Original, 0o600); err != nil {
				return fmt.Errorf("create exact backup %s: %w", p.Backup, err)
			}
		} else {
			return fmt.Errorf("inspect backup %s: %w", p.Backup, err)
		}
	}
	if err := writeAtomic(p.Path, p.Updated, 0o600); err != nil {
		return fmt.Errorf("write Codex configuration %s: %w", p.Path, err)
	}
	return nil
}

// checkCurrent prevents a delayed post-health apply from overwriting a Codex
// configuration that changed after Prepare. It is deliberately a narrow
// compare-and-refuse check, not a cross-process locking scheme.
func (p Plan) checkCurrent() error {
	current, err := os.ReadFile(p.Path)
	if p.OriginalExists {
		if err != nil || !bytes.Equal(current, p.Original) {
			return fmt.Errorf("Codex configuration changed since it was prepared; rerun setup --configure-codex")
		}
		return nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err == nil {
		return fmt.Errorf("Codex configuration changed since it was prepared; rerun setup --configure-codex")
	}
	return fmt.Errorf("check Codex configuration %s before applying changes: %w", p.Path, err)
}

type edit struct {
	start int
	end   int
	data  []byte
}

func rootEdits(data []byte, baseURL, catalogPath string) ([]edit, int, bool, error) {
	var parser unstable.Parser
	parser.Reset(data)
	section := []string(nil)
	firstTable := len(data)
	seen := map[string]bool{}
	edits := make([]edit, 0, 2)
	values := map[string]string{
		"openai_base_url":    baseURL,
		"model_catalog_json": catalogPath,
	}

	for parser.NextExpression() {
		node := parser.Expression()
		keys := nodeKeys(node)
		switch node.Kind {
		case unstable.Table, unstable.ArrayTable:
			section = keys
			if firstTable == len(data) {
				firstTable = int(node.Raw.Offset)
			}
			if len(keys) > 0 && keys[0] == "model_provider" {
				return nil, 0, true, nil
			}
		case unstable.KeyValue:
			if len(section) != 0 || len(keys) == 0 {
				continue
			}
			if keys[0] == "model_provider" {
				return nil, 0, true, nil
			}
			if len(keys) != 1 {
				continue
			}
			value, wanted := values[keys[0]]
			if !wanted {
				continue
			}
			if seen[keys[0]] {
				return nil, 0, false, fmt.Errorf("root key %q appears more than once", keys[0])
			}
			seen[keys[0]] = true
			valueRange := node.Value().Raw
			edits = append(edits, edit{start: int(valueRange.Offset), end: int(valueRange.Offset + valueRange.Length), data: []byte(strconv.Quote(value))})
		}
	}
	if err := parser.Error(); err != nil {
		return nil, 0, false, err
	}

	missing := make([]string, 0, 2)
	for _, key := range []string{"openai_base_url", "model_catalog_json"} {
		if !seen[key] {
			missing = append(missing, key+" = "+strconv.Quote(values[key]))
		}
	}
	if len(missing) > 0 {
		insert := []byte(strings.Join(missing, "\n") + "\n")
		if firstTable > 0 && len(data) > 0 && data[firstTable-1] != '\n' {
			insert = append([]byte("\n"), insert...)
		}
		if firstTable == len(data) && len(data) > 0 && data[len(data)-1] != '\n' {
			insert = append([]byte("\n"), insert...)
		}
		edits = append(edits, edit{start: firstTable, end: firstTable, data: insert})
	}
	return edits, firstTable, false, nil
}

func nodeKeys(node *unstable.Node) []string {
	keys := make([]string, 0, 2)
	it := node.Key()
	for it.Next() {
		keys = append(keys, string(it.Node().Data))
	}
	return keys
}

func applyEdits(data []byte, edits []edit, _ int) []byte {
	for i := 1; i < len(edits); i++ {
		for j := i; j > 0 && edits[j-1].start < edits[j].start; j-- {
			edits[j-1], edits[j] = edits[j], edits[j-1]
		}
	}
	updated := append([]byte(nil), data...)
	for _, current := range edits {
		updated = append(updated[:current.start], append(current.data, updated[current.end:]...)...)
	}
	return updated
}

func validateTOML(data []byte) error {
	var values map[string]any
	return toml.Unmarshal(data, &values)
}

func writeNewFile(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	name := file.Name()
	defer func() { _ = file.Close() }()
	if _, err := file.Write(data); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	return file.Close()
}

func writeAtomic(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".codex-model-router-*")
	if err != nil {
		return err
	}
	name := temp.Name()
	defer func() {
		_ = temp.Close()
		_ = os.Remove(name)
	}()
	if _, err := temp.Write(data); err != nil {
		return err
	}
	if err := temp.Chmod(mode); err != nil {
		return err
	}
	if err := temp.Sync(); err != nil {
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}
