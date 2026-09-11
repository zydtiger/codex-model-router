package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Runner executes lifecycle commands. Tests replace it to check the exact
// launchctl sequence without touching a real launchd domain.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

// ExecRunner runs commands as a subprocess.
type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, name, args...)
	output, err := command.CombinedOutput()
	if err != nil {
		return output, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return output, nil
}

// Manager installs, inspects, and removes the LaunchAgent for one configuration.
// It never uses sudo: everything happens in the caller's own gui domain and under
// the caller's own home directory.
type Manager struct {
	Options Options
	Runner  Runner
	// HealthProbe checks the running router. Tests substitute a stub.
	HealthProbe func(ctx context.Context) (map[string]any, error)
	// now and write are overridable for tests.
	writeFile  func(path string, data []byte, mode os.FileMode) error
	removeFile func(path string) error
}

// Step is one lifecycle action, recorded so callers can print exactly what
// happened or would happen.
type Step struct {
	Action  string
	Detail  string
	Skipped bool
}

// Report is the outcome of an install, status, or uninstall call.
type Report struct {
	UnitPath  string
	PlistPath string
	Steps     []Step
	Notes     []string
	Health    map[string]any
}

// Describe renders the report for a terminal.
func (r Report) Describe() string {
	lines := make([]string, 0, len(r.Steps)+len(r.Notes)+1)
	if r.UnitPath != "" {
		lines = append(lines, "systemd user service: "+r.UnitPath)
	} else {
		lines = append(lines, "LaunchAgent: "+r.PlistPath)
	}
	for _, step := range r.Steps {
		if step.Skipped {
			lines = append(lines, "  skipped  "+step.Action+" ("+step.Detail+")")
			continue
		}
		if step.Detail == "" {
			lines = append(lines, "  done     "+step.Action)
			continue
		}
		lines = append(lines, "  done     "+step.Action+" — "+step.Detail)
	}
	for _, note := range r.Notes {
		lines = append(lines, "  note     "+note)
	}
	return strings.Join(lines, "\n") + "\n"
}

func (m *Manager) runner() Runner {
	if m.Runner != nil {
		return m.Runner
	}
	return ExecRunner{}
}

func (m *Manager) resolved() (Resolved, error) {
	resolved, err := m.Options.Resolve()
	if err != nil {
		return Resolved{}, err
	}
	if resolved.UID == 0 {
		resolved.UID = os.Getuid()
	}
	return resolved, nil
}

// Preview renders the plist without touching anything. It is what `service
// preview` prints and what `service install --dry-run` shows first.
func (m *Manager) Preview() (string, Resolved, error) {
	if m.platform() == "linux" {
		rendered, path, err := m.systemdPreview()
		return rendered, Resolved{Options: m.Options, UnitPath: path}, err
	}
	if m.platform() != "darwin" {
		return "", Resolved{}, fmt.Errorf("unsupported service platform %q", m.platform())
	}
	resolved, err := m.resolved()
	if err != nil {
		return "", Resolved{}, err
	}
	rendered, err := RenderLaunchAgent(m.Options)
	if err != nil {
		return "", Resolved{}, err
	}
	return rendered, resolved, nil
}

// Install writes the plist and loads it into the user's launchd domain.
//
// dryRun performs every check and prints the plan without writing or loading. An
// existing file at the target path is only replaced when it is recognisably this
// service's own file, or when allowOverwrite is set.
func (m *Manager) Install(ctx context.Context, dryRun, allowOverwrite bool) (Report, error) {
	if m.platform() == "linux" {
		return m.systemdInstall(ctx, dryRun, allowOverwrite)
	}
	if m.platform() != "darwin" {
		return Report{}, fmt.Errorf("unsupported service platform %q", m.platform())
	}
	resolved, err := m.resolved()
	if err != nil {
		return Report{}, err
	}
	report := Report{PlistPath: resolved.PlistPath}

	if err := m.checkBinary(resolved.BinaryPath); err != nil {
		return report, err
	}
	if _, err := os.Stat(resolved.ConfigPath); err != nil {
		return report, fmt.Errorf("the agent needs a readable config at %s: %w", resolved.ConfigPath, err)
	}

	rendered, err := RenderLaunchAgent(m.Options)
	if err != nil {
		return report, err
	}

	mode := os.FileMode(0o644)
	if len(resolved.Env) > 0 {
		// The plist may carry an API key, so it is not world readable.
		mode = 0o600
	}

	existing, readErr := os.ReadFile(resolved.PlistPath)
	switch {
	case readErr == nil:
		if !Owned(existing, resolved.Label, resolved.BinaryPath) && !allowOverwrite {
			return report, fmt.Errorf("%s exists and is not managed by this tool; not overwriting (pass --force to replace it)", resolved.PlistPath)
		}
		report.Steps = append(report.Steps, Step{Action: "replace plist", Detail: resolved.PlistPath})
	case errors.Is(readErr, os.ErrNotExist):
		report.Steps = append(report.Steps, Step{Action: "write plist", Detail: resolved.PlistPath})
	default:
		return report, readErr
	}
	report.Steps = append(report.Steps,
		Step{Action: "create log directory", Detail: filepath.Dir(resolved.StdOutPath)},
		Step{Action: "launchctl bootout", Detail: resolved.ServiceTarget()},
		Step{Action: "launchctl bootstrap", Detail: resolved.Domain() + " " + resolved.PlistPath},
		Step{Action: "launchctl kickstart", Detail: resolved.ServiceTarget()},
	)
	if dryRun {
		report.Notes = append(report.Notes, "dry run: nothing was written or loaded")
		return report, nil
	}

	// A previously loaded copy of this label has to be cleared before bootstrap will
	// accept the new definition. This happens before anything is written so a real
	// launchd failure leaves the existing files alone; an agent that was simply not
	// loaded is not an error.
	if _, err := m.runner().Run(ctx, "launchctl", "bootout", resolved.ServiceTarget()); err != nil {
		if notLoadedMessage(err) {
			report.Notes = append(report.Notes, "no agent was loaded for "+resolved.ServiceTarget())
		} else {
			return report, fmt.Errorf("launchctl bootout %s failed and nothing was written: %w", resolved.ServiceTarget(), err)
		}
	}

	if err := os.MkdirAll(resolved.PlistDir, 0o755); err != nil {
		return report, err
	}
	if err := os.MkdirAll(filepath.Dir(resolved.StdOutPath), 0o755); err != nil {
		return report, err
	}
	if err := m.writeFileFunc()(resolved.PlistPath, []byte(rendered), mode); err != nil {
		return report, err
	}

	if err := m.bootstrap(ctx, resolved); err != nil {
		return report, fmt.Errorf("bootstrap failed and %s was left in place: %w", resolved.PlistPath, err)
	}
	if _, err := m.runner().Run(ctx, "launchctl", "kickstart", "-k", resolved.ServiceTarget()); err != nil {
		return report, fmt.Errorf("the agent was installed but kickstart failed: %w", err)
	}
	report.Notes = append(report.Notes,
		"the agent starts at login and relaunches after a crash",
		"edit the Codex configuration yourself or through your agent, then restart Codex; see docs/codex-desktop.md",
	)
	return report, nil
}

// Uninstall stops the agent and removes its plist. It deliberately leaves the
// router configuration, the catalog, and any Codex config alone: those are user
// files with their own restore path.
func (m *Manager) Uninstall(ctx context.Context, dryRun, allowOverwrite bool) (Report, error) {
	if m.platform() == "linux" {
		return m.systemdUninstall(ctx, dryRun, allowOverwrite)
	}
	if m.platform() != "darwin" {
		return Report{}, fmt.Errorf("unsupported service platform %q", m.platform())
	}
	resolved, err := m.resolved()
	if err != nil {
		return Report{}, err
	}
	report := Report{PlistPath: resolved.PlistPath}
	existing, readErr := os.ReadFile(resolved.PlistPath)
	switch {
	case errors.Is(readErr, os.ErrNotExist):
		report.Steps = append(report.Steps, Step{Action: "remove plist", Detail: resolved.PlistPath, Skipped: true})
		report.Notes = append(report.Notes, "no LaunchAgent is installed at that path")
		return report, nil
	case readErr != nil:
		return report, readErr
	}
	if !Owned(existing, resolved.Label, resolved.BinaryPath) && !allowOverwrite {
		return report, fmt.Errorf("%s is not managed by this tool; not removing it (pass --force to remove anyway)", resolved.PlistPath)
	}
	report.Steps = append(report.Steps,
		Step{Action: "launchctl bootout", Detail: resolved.ServiceTarget()},
		Step{Action: "remove plist", Detail: resolved.PlistPath},
	)
	if dryRun {
		report.Notes = append(report.Notes, "dry run: nothing was removed")
		return report, nil
	}
	if _, err := m.runner().Run(ctx, "launchctl", "bootout", resolved.ServiceTarget()); err != nil {
		// An agent that was never loaded is the expected state for a plist left behind
		// by an interrupted install, so the file can still be removed. Anything else is
		// a real launchd failure: the plist stays so the agent can be retried, and the
		// failure is returned rather than reported as success.
		if !notLoadedMessage(err) {
			return report, fmt.Errorf("launchctl bootout %s failed and %s was kept so the agent can be retried: %w",
				resolved.ServiceTarget(), resolved.PlistPath, err)
		}
		report.Notes = append(report.Notes, "no agent was loaded for "+resolved.ServiceTarget())
	}
	if err := m.removeFileFunc()(resolved.PlistPath); err != nil {
		return report, fmt.Errorf("the agent was stopped but %s could not be removed: %w", resolved.PlistPath, err)
	}
	report.Notes = append(report.Notes,
		"configuration and catalog files were left untouched; undo Codex configuration edits yourself or through your agent",
	)
	return report, nil
}

// Status asks launchd whether the agent is loaded and probes the router health
// endpoint. Both signals are reported because a loaded agent is not a healthy
// router.
func (m *Manager) Status(ctx context.Context) (Report, error) {
	if m.platform() == "linux" {
		return m.systemdStatus(ctx)
	}
	if m.platform() != "darwin" {
		return Report{}, fmt.Errorf("unsupported service platform %q", m.platform())
	}
	resolved, err := m.resolved()
	if err != nil {
		return Report{}, err
	}
	report := Report{PlistPath: resolved.PlistPath}

	if _, err := os.Stat(resolved.PlistPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			report.Notes = append(report.Notes, "not installed: no plist at "+resolved.PlistPath)
		} else {
			return report, err
		}
	} else {
		content, readErr := os.ReadFile(resolved.PlistPath)
		if readErr != nil {
			return report, readErr
		}
		if Owned(content, resolved.Label, resolved.BinaryPath) {
			report.Steps = append(report.Steps, Step{Action: "plist present", Detail: "managed by this tool"})
		} else {
			report.Steps = append(report.Steps, Step{Action: "plist present", Detail: "written by something else"})
		}
	}

	output, err := m.runner().Run(ctx, "launchctl", "print", resolved.ServiceTarget())
	switch {
	case err == nil:
		report.Steps = append(report.Steps, Step{Action: "launchd state", Detail: summarizeLaunchdOutput(string(output))})
	case notLoadedMessage(err):
		report.Steps = append(report.Steps, Step{Action: "launchd state", Detail: "not loaded"})
	default:
		report.Steps = append(report.Steps, Step{Action: "launchd state", Detail: "unknown (" + firstLine(err) + ")"})
	}

	if m.HealthProbe != nil {
		health, healthErr := m.HealthProbe(ctx)
		if healthErr != nil {
			report.Notes = append(report.Notes, "health probe failed: "+healthErr.Error())
			return report, nil
		}
		report.Health = health
		report.Steps = append(report.Steps, Step{Action: "health", Detail: summarizeHealth(health)})
	}
	return report, nil
}

// CheckBinary validates the file the agent would exec.
func (m *Manager) checkBinary(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("the agent binary %s: %w", path, err)
	}
	if info.IsDir() {
		return fmt.Errorf("%s is a directory", path)
	}
	if info.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("%s is not executable", path)
	}
	return nil
}

func (m *Manager) writeFileFunc() func(string, []byte, os.FileMode) error {
	if m.writeFile != nil {
		return m.writeFile
	}
	return writeFileAtomic
}

func (m *Manager) removeFileFunc() func(string) error {
	if m.removeFile != nil {
		return m.removeFile
	}
	return os.Remove
}

// writeFileAtomic replaces a file through a temporary file in the same directory.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
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

// DefaultPlistDir is the user LaunchAgents directory.
func DefaultPlistDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents"), nil
}

// DefaultLogDir is where the agent's log files live.
func DefaultLogDir(label string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "Logs", label), nil
}

// DefaultInstallDir is the stable location for the executable the agent runs.
// Keeping it outside any checkout means an upgrade of the checkout cannot break a
// running agent.
func DefaultInstallDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "lib", "codex-model-router"), nil
}

// DefaultBinaryPath is the stable installed binary path.
func DefaultBinaryPath() (string, error) {
	directory, err := DefaultInstallDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(directory, "codex-model-router"), nil
}

func firstLine(err error) string {
	if err == nil {
		return ""
	}
	line, _, _ := strings.Cut(strings.TrimSpace(err.Error()), "\n")
	return line
}

func notLoadedMessage(err error) bool {
	message := strings.ToLower(safeErrorMessage(err))
	// These are the ways launchctl says "that service is not loaded here". A missing
	// launchctl binary or an I/O failure must not match, because those are real
	// failures and must stop a removal.
	for _, marker := range []string{
		"could not find service",
		"no such service",
		"no such process",
		"already bootouted",
		"is not found in the requested domain",
		"is not loaded",
	} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}

// safeErrorMessage keeps launchd output readable. launchctl errors quote the plist
// path and the label, both of which are already known to the caller.
func safeErrorMessage(err error) string {
	if err == nil {
		return ""
	}
	return strings.TrimSpace(err.Error())
}

func summarizeLaunchdOutput(output string) string {
	fields := map[string]string{}
	for _, line := range strings.Split(output, "\n") {
		key, value, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		fields[strings.TrimSpace(key)] = strings.TrimSpace(value)
	}
	var parts []string
	for _, key := range []string{"state", "pid", "last exit code", "run type"} {
		if value, ok := fields[key]; ok && value != "" {
			parts = append(parts, key+"="+value)
		}
	}
	if len(parts) == 0 {
		return "loaded"
	}
	sort.Strings(parts)
	return strings.Join(parts, " ")
}

func summarizeHealth(health map[string]any) string {
	if health == nil {
		return "no data"
	}
	var parts []string
	for _, key := range []string{"ok", "version", "bound", "requests", "remote_models", "native_models", "upstream_errors"} {
		if value, ok := health[key]; ok {
			parts = append(parts, key+"="+formatValue(value))
		}
	}
	return strings.Join(parts, " ")
}

func formatValue(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(typed)
	default:
		return fmt.Sprintf("%v", typed)
	}
}

func (m *Manager) platform() string {
	if m.Options.Platform != "" {
		return m.Options.Platform
	}
	return runtime.GOOS
}

// launchd can acknowledge bootout before releasing the service registration.
// Retry only its observed bootstrap EIO response, with a short bounded backoff.
func (m *Manager) bootstrap(ctx context.Context, r Resolved) error {
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		_, err := m.runner().Run(ctx, "launchctl", "bootstrap", r.Domain(), r.PlistPath)
		if err == nil {
			return nil
		}
		if attempt == 4 || !strings.Contains(strings.ToLower(err.Error()), "bootstrap failed: 5: input/output error") {
			return err
		}
		timer := time.NewTimer(100 * time.Millisecond << attempt)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
