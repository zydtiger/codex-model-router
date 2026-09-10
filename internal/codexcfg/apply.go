// Package codexcfg updates the root keys Codex needs to use the router.
//
// Only two root keys make the router take effect: openai_base_url, which points
// Codex at the loopback listener, and model_catalog_json, which points it at the
// combined catalog. Everything else in config.toml, including comments, layout,
// and unrelated tables, is left exactly as it was.
package codexcfg

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
)

// Key names in Codex's config.toml.
const (
	KeyOpenAIBaseURL = "openai_base_url"
	KeyModelCatalog  = "model_catalog_json"
	KeyModelProvider = "model_provider"
	KeyModel         = "model"
)

// Action describes what a plan will do to one key.
type Action string

const (
	ActionAdd       Action = "add"
	ActionChange    Action = "change"
	ActionRemove    Action = "remove"
	ActionUnchanged Action = "unchanged"
)

// Change is one planned key edit, for display before anything is written.
type Change struct {
	Key      string
	Action   Action
	From     string
	To       string
	Required bool
	Note     string
}

// Plan is the result of inspecting a Codex config file. It is inert: producing a
// plan never writes.
type Plan struct {
	Path           string
	Existed        bool
	Changes        []Change
	UpdatedText    string
	Notes          []string
	AlreadyCurrent bool

	// sourceFingerprint identifies the exact bytes UpdatedText was derived from.
	// Apply compares it against the file again so an edit made between planning and
	// confirming cannot be silently overwritten.
	sourceFingerprint string
}

// fingerprint identifies a file's content, or its absence.
func fingerprint(text string, existed bool) string {
	if !existed {
		return "absent"
	}
	return fmt.Sprintf("%x", sha256.Sum256([]byte(text)))
}

// Empty reports whether the plan has nothing to write.
func (p Plan) Empty() bool {
	for _, change := range p.Changes {
		if change.Action != ActionUnchanged {
			return false
		}
	}
	return true
}

// Describe renders the plan for a human reviewing it before --confirm.
func (p Plan) Describe() string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "Codex config: %s\n", p.Path)
	if !p.Existed {
		fmt.Fprintf(&builder, "  (file does not exist yet; it will be created)\n")
	}
	if len(p.Changes) == 0 {
		builder.WriteString("  no changes\n")
	}
	for _, change := range p.Changes {
		switch change.Action {
		case ActionUnchanged:
			fmt.Fprintf(&builder, "  = %-18s %s\n", change.Key, change.To)
		case ActionAdd:
			fmt.Fprintf(&builder, "  + %-18s %s\n", change.Key, change.To)
		case ActionChange:
			fmt.Fprintf(&builder, "  ~ %-18s %s -> %s\n", change.Key, change.From, change.To)
		case ActionRemove:
			fmt.Fprintf(&builder, "  - %-18s %s%s\n", change.Key, change.From, noteSuffix(change.Note))
		}
		if change.Action != ActionUnchanged && change.Note != "" && change.Action != ActionRemove {
			fmt.Fprintf(&builder, "      note: %s\n", change.Note)
		}
	}
	for _, note := range p.Notes {
		fmt.Fprintf(&builder, "  note: %s\n", note)
	}
	return builder.String()
}

func noteSuffix(note string) string {
	if note == "" {
		return ""
	}
	return "  (" + note + ")"
}

// Desired is the target state for the two router keys.
type Desired struct {
	// OpenAIBaseURL is the loopback router URL, for example
	// http://127.0.0.1:4317/v1.
	OpenAIBaseURL string
	// ModelCatalogJSON is the absolute path of the combined catalog file.
	ModelCatalogJSON string
}

// Validate checks the values before they are written anywhere.
func (d Desired) Validate() error {
	if strings.TrimSpace(d.OpenAIBaseURL) == "" {
		return errors.New("openai_base_url value is empty")
	}
	if err := validateLoopbackURL(d.OpenAIBaseURL); err != nil {
		return err
	}
	if strings.ContainsAny(d.OpenAIBaseURL, "\r\n\"\\") {
		return errors.New("openai_base_url contains characters that cannot be written safely")
	}
	if strings.TrimSpace(d.ModelCatalogJSON) == "" {
		return errors.New("model_catalog_json value is empty")
	}
	if !filepath.IsAbs(d.ModelCatalogJSON) {
		return fmt.Errorf("model_catalog_json %q must be an absolute path", d.ModelCatalogJSON)
	}
	if strings.ContainsAny(d.ModelCatalogJSON, "\r\n\"\\") {
		return errors.New("model_catalog_json contains characters that cannot be written safely")
	}
	return nil
}

// BuildPlan reads the current config file and reports the edits that would make
// it point at the router.
func BuildPlan(path string, desired Desired) (Plan, error) {
	if err := desired.Validate(); err != nil {
		return Plan{}, err
	}
	plan := Plan{Path: path}
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		plan.Existed = true
	case os.IsNotExist(err):
	default:
		return Plan{}, fmt.Errorf("read %s: %w", path, err)
	}
	text := string(data)

	// A file that cannot be parsed is never rewritten: an unsafe edit of a
	// config Codex itself cannot read would look like a router failure.
	if plan.Existed {
		if _, err := toml.Decode(text, &map[string]any{}); err != nil {
			return Plan{}, fmt.Errorf("%s is not valid TOML, refusing to edit it: %w", path, err)
		}
	}

	root, err := parseRoot(text)
	if err != nil {
		return Plan{}, fmt.Errorf("%s: %w", path, err)
	}

	currentBase := root.value(KeyOpenAIBaseURL)
	if currentBase == nil || currentBase.kind != kindString || currentBase.value != desired.OpenAIBaseURL {
		plan.Changes = append(plan.Changes, Change{
			Key:      KeyOpenAIBaseURL,
			Action:   planAction(currentBase),
			From:     describeValue(currentBase),
			To:       strconv.Quote(desired.OpenAIBaseURL),
			Required: true,
			Note:     "all model requests go through the local router",
		})
	} else {
		plan.Changes = append(plan.Changes, Change{
			Key:    KeyOpenAIBaseURL,
			Action: ActionUnchanged,
			To:     strconv.Quote(desired.OpenAIBaseURL),
		})
	}

	currentCatalog := root.value(KeyModelCatalog)
	if currentCatalog == nil || currentCatalog.kind != kindString || currentCatalog.value != desired.ModelCatalogJSON {
		plan.Changes = append(plan.Changes, Change{
			Key:      KeyModelCatalog,
			Action:   planAction(currentCatalog),
			From:     describeValue(currentCatalog),
			To:       strconv.Quote(desired.ModelCatalogJSON),
			Required: true,
			Note:     "combined native and self-hosted model picker",
		})
	} else {
		plan.Changes = append(plan.Changes, Change{
			Key:    KeyModelCatalog,
			Action: ActionUnchanged,
			To:     strconv.Quote(desired.ModelCatalogJSON),
		})
	}

	// model_provider selects one provider and would bypass the native ChatGPT
	// session, so it must not be set for this integration.
	if provider := root.value(KeyModelProvider); provider != nil {
		plan.Changes = append(plan.Changes, Change{
			Key:      KeyModelProvider,
			Action:   ActionRemove,
			From:     describeValue(provider),
			Required: true,
			Note:     "leaving model_provider unset keeps native account authentication",
		})
	}

	if model := root.value(KeyModel); model == nil {
		plan.Notes = append(plan.Notes, "no default model is set; pick one in the Codex model selector after restarting Codex")
	}

	updated := text
	if currentBase == nil || currentBase.value != desired.OpenAIBaseURL || currentBase.kind != kindString {
		updated, err = setRootString(updated, KeyOpenAIBaseURL, desired.OpenAIBaseURL)
		if err != nil {
			return Plan{}, fmt.Errorf("%s: %w", path, err)
		}
	}
	if currentCatalog == nil || currentCatalog.value != desired.ModelCatalogJSON || currentCatalog.kind != kindString {
		updated, err = setRootString(updated, KeyModelCatalog, desired.ModelCatalogJSON)
		if err != nil {
			return Plan{}, fmt.Errorf("%s: %w", path, err)
		}
	}
	if root.value(KeyModelProvider) != nil {
		updated, err = removeRootKey(updated, KeyModelProvider)
		if err != nil {
			return Plan{}, fmt.Errorf("%s: %w", path, err)
		}
	}
	if err := verifyUpdate(text, updated, desired); err != nil {
		return Plan{}, fmt.Errorf("%s: %w", path, err)
	}
	plan.UpdatedText = updated
	plan.AlreadyCurrent = plan.Empty()
	plan.sourceFingerprint = fingerprint(text, plan.Existed)
	return plan, nil
}

// verifyUpdate parses the before and after text and requires that exactly the
// intended root keys differ. This is what makes the line edits safe to apply to a
// file with unrelated tables and comments.
func verifyUpdate(before, after string, desired Desired) error {
	var previous, current map[string]any
	if strings.TrimSpace(before) != "" {
		if err := toml.Unmarshal([]byte(before), &previous); err != nil {
			return fmt.Errorf("the current file does not parse: %w", err)
		}
	}
	if err := toml.Unmarshal([]byte(after), &current); err != nil {
		return fmt.Errorf("the updated file does not parse: %w", err)
	}
	want := map[string]any{
		KeyOpenAIBaseURL: desired.OpenAIBaseURL,
		KeyModelCatalog:  desired.ModelCatalogJSON,
	}
	for key, expected := range want {
		if got, ok := current[key]; !ok {
			return fmt.Errorf("%s disappeared while updating", key)
		} else if got != expected {
			return fmt.Errorf("%s was written as %v", key, got)
		}
	}
	if _, ok := current[KeyModelProvider]; ok {
		return errors.New("model_provider is still set after the update")
	}
	// Removing a previous model_provider is intended: it is what keeps native
	// account authentication working.
	ignore := map[string]any{KeyModelProvider: nil}
	for key, value := range want {
		ignore[key] = value
	}
	if !equalIgnoringKeys(previous, current, ignore) {
		return errors.New("the update changed keys other than openai_base_url and model_catalog_json")
	}
	return nil
}

// equalIgnoringKeys compares two parsed tables except for the intended keys.
// Unparseable values cannot appear because both sides already decoded.
func equalIgnoringKeys(previous, current map[string]any, ignore map[string]any) bool {
	strip := func(table map[string]any) string {
		copied := make(map[string]any, len(table))
		for key, value := range table {
			if _, skip := ignore[key]; skip {
				continue
			}
			copied[key] = value
		}
		encoded, err := json.Marshal(copied)
		if err != nil {
			// TOML decodes into JSON-compatible values, so this means the
			// comparison itself failed and the update must not proceed.
			return "\x00marshal-failed" + err.Error()
		}
		return string(encoded)
	}
	return strip(previous) == strip(current)
}

// ErrConfirmationRequired is returned when a plan is handed to Apply without
// confirmation. A caller can map it to a usage exit code without matching text.
var ErrConfirmationRequired = errors.New("confirmation required")

// Apply writes a plan. It refuses to write an unconfirmed plan, keeps a backup
// next to the file, and restores that backup when the write or the verification
// afterwards fails. The backup path is returned even when the write fails, so the
// caller can point at it.
func Apply(plan Plan, confirm bool) (backupPath string, err error) {
	if !confirm {
		return "", fmt.Errorf("%w: refusing to write; pass --confirm after reviewing the plan", ErrConfirmationRequired)
	}
	if plan.Empty() {
		return "", nil
	}
	if strings.TrimSpace(plan.Path) == "" {
		return "", errors.New("no config path")
	}

	// The file is read again before anything is written. A plan describes a
	// specific revision of the file, so an edit made while the plan was under
	// review stops the write instead of being overwritten by it.
	current, exists, err := readTextFile(plan.Path)
	if err != nil {
		return "", err
	}
	if fingerprint(current, exists) != plan.sourceFingerprint {
		return "", fmt.Errorf("%s changed after this plan was built; nothing was written. Run the plan again to review the new content", plan.Path)
	}

	if err := os.MkdirAll(filepath.Dir(plan.Path), 0o755); err != nil {
		return "", err
	}

	backup := ""
	if plan.Existed {
		backup = BackupPath(plan.Path)
		mode := os.FileMode(0o600)
		if info, statErr := os.Stat(plan.Path); statErr == nil {
			mode = info.Mode().Perm()
		}
		if err := writeFile(plan.Path+".codex-model-router.tmp", []byte(current), mode); err != nil {
			return "", err
		}
		if err := os.Rename(plan.Path+".codex-model-router.tmp", backup); err != nil {
			return "", fmt.Errorf("write backup %s: %w", backup, err)
		}
	}

	mode := os.FileMode(0o600)
	if info, statErr := os.Stat(plan.Path); statErr == nil {
		mode = info.Mode().Perm()
	}
	if err := writeFile(plan.Path, []byte(plan.UpdatedText), mode); err != nil {
		if backup != "" {
			_ = copyFile(backup, plan.Path)
		}
		return backup, err
	}
	if _, err := toml.Decode(plan.UpdatedText, &map[string]any{}); err != nil {
		if backup != "" {
			_ = copyFile(backup, plan.Path)
		}
		return backup, fmt.Errorf("the updated file does not parse; restored %s: %w", backup, err)
	}
	return backup, nil
}

// readTextFile returns a file's content and whether it exists. A missing file is
// not an error: an absent config is a case this package handles.

func readTextFile(path string) (string, bool, error) {
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		return string(data), true, nil
	case errors.Is(err, os.ErrNotExist):
		return "", false, nil
	default:
		return "", false, fmt.Errorf("read %s: %w", path, err)
	}
}

// validateLoopbackURL keeps openai_base_url pointed at the router and nowhere
// else. The value is written into a config file that every model request reads,
// so a non-loopback or malformed URL is refused instead of trusted.
func validateLoopbackURL(value string) error {
	parsed, err := url.Parse(value)
	if err != nil {
		return fmt.Errorf("openai_base_url %q is not a valid URL: %w", value, err)
	}
	if parsed.Scheme != "http" {
		return fmt.Errorf("openai_base_url %q must use the http scheme: the router serves loopback http", value)
	}
	if parsed.User != nil {
		return fmt.Errorf("openai_base_url %q must not embed credentials", value)
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("openai_base_url %q must not include a query or fragment", value)
	}
	if parsed.Host == "" {
		return fmt.Errorf("openai_base_url %q must include a host and port", value)
	}
	host, port, err := net.SplitHostPort(parsed.Host)
	if err != nil {
		return fmt.Errorf("openai_base_url %q must name a host and port, for example http://127.0.0.1:4317/v1", value)
	}
	number, err := strconv.Atoi(port)
	if err != nil || number < 1 || number > 65535 {
		return fmt.Errorf("openai_base_url %q has an invalid port %q", value, port)
	}
	if !isLoopbackHost(host) {
		return fmt.Errorf("openai_base_url %q must point at a loopback address, got host %q", value, host)
	}
	return nil
}

// isLoopbackHost accepts the loopback literals only. A name is not resolved: this
// runs before any network access, and resolving a config value here would let a
// hostname stand in for the loopback contract.
func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		host = host[1 : len(host)-1]
	}
	address := net.ParseIP(host)
	return address != nil && address.IsLoopback()
}

// Restore puts a backup file back over the config path.
func Restore(path, backupPath string) error {
	if strings.TrimSpace(path) == "" || strings.TrimSpace(backupPath) == "" {
		return errors.New("both the config path and the backup path are required")
	}
	if _, err := os.Stat(backupPath); err != nil {
		return fmt.Errorf("backup %s: %w", backupPath, err)
	}
	data, err := os.ReadFile(backupPath)
	if err != nil {
		return err
	}
	info, err := os.Stat(path)
	mode := os.FileMode(0o600)
	if err == nil {
		mode = info.Mode().Perm()
	}
	return writeFile(path, data, mode)
}

// Snippet is the documented manual configuration. It is the recommended path for
// anyone who would rather edit config.toml themselves than run an apply step.
func Snippet(desired Desired) string {
	return strings.Join([]string{
		"# Codex desktop configuration for codex-model-router.",
		"# Keep model_provider unset: that is what preserves native ChatGPT",
		"# authentication for the bundled models while the router serves the rest.",
		fmt.Sprintf("%s = %s", KeyOpenAIBaseURL, strconv.Quote(desired.OpenAIBaseURL)),
		fmt.Sprintf("%s = %s", KeyModelCatalog, strconv.Quote(desired.ModelCatalogJSON)),
	}, "\n") + "\n"
}

// BackupPath is where Apply stores the previous file contents.
func BackupPath(path string) string { return path + ".codex-model-router.bak" }

func planAction(current *rootValue) Action {
	if current == nil {
		return ActionAdd
	}
	return ActionChange
}

func describeValue(value *rootValue) string {
	if value == nil {
		return "(unset)"
	}
	return value.raw
}

func writeFile(path string, data []byte, mode os.FileMode) error {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return err
	}
	temp, err := os.CreateTemp(directory, ".codex-model-router-*")
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

func copyFile(from, to string) error {
	data, err := os.ReadFile(from)
	if err != nil {
		return err
	}
	info, err := os.Stat(from)
	mode := os.FileMode(0o600)
	if err == nil {
		mode = info.Mode().Perm()
	}
	return writeFile(to, data, mode)
}

// sortedKeys is used by tests and diagnostics for deterministic output.
func sortedKeys(table map[string]any) []string {
	keys := make([]string, 0, len(table))
	for key := range table {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
