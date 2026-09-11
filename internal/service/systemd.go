package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// DefaultUnitDir respects XDG_CONFIG_HOME for systemd's user unit search path.
func DefaultUnitDir() (string, error) {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".config")
	}
	if !filepath.IsAbs(base) {
		return "", errors.New("XDG_CONFIG_HOME must be absolute")
	}
	return filepath.Join(base, "systemd", "user"), nil
}

// unitQuote escapes systemd syntax, including specifiers. ExecStart uses the ':'
// prefix to disable environment substitution, so dollar signs remain literal.
func unitQuote(value string) (string, error) {
	for _, c := range value {
		if c < 0x20 || c == 0x7f {
			return "", errors.New("systemd values must not contain control characters")
		}
	}
	value = strings.NewReplacer("\\", "\\\\", "\"", "\\\"", "%", "%%").Replace(value)
	return "\"" + value + "\"", nil
}

func (m *Manager) systemdPreview() (string, string, error) {
	o := m.Options
	label := strings.TrimSpace(o.Label)
	if label == "" {
		label = DefaultLabel
	}
	if err := validLabel(label); err != nil {
		return "", "", err
	}
	if o.UnitDir == "" {
		var err error
		o.UnitDir, err = DefaultUnitDir()
		if err != nil {
			return "", "", err
		}
	}
	if o.PlistDir != "" || o.LogDir != "" {
		return "", "", errors.New("--plist-dir and --log-dir apply only to macOS; systemd logs go to the journal")
	}
	if o.WorkingDirectory == "" {
		var err error
		o.WorkingDirectory, err = os.UserHomeDir()
		if err != nil {
			return "", "", err
		}
	}
	for _, path := range []string{o.BinaryPath, o.ConfigPath, o.UnitDir, o.WorkingDirectory} {
		if !filepath.IsAbs(path) {
			return "", "", errors.New("systemd binary, config, unit directory, and working directory paths must be absolute")
		}
	}
	if o.ThrottleInterval < 0 {
		return "", "", errors.New("throttle interval must not be negative")
	}
	if o.ThrottleInterval == 0 {
		o.ThrottleInterval = 10
	}
	args := append([]string{o.BinaryPath, "serve", "--config", o.ConfigPath}, o.ExtraArgs...)
	for i, arg := range args {
		quoted, err := unitQuote(arg)
		if err != nil {
			return "", "", err
		}
		args[i] = quoted
	}
	_, err := unitQuote(o.WorkingDirectory)
	if err != nil {
		return "", "", err
	}
	if strings.TrimSpace(o.WorkingDirectory) != o.WorkingDirectory {
		return "", "", errors.New("systemd working directory must not have leading or trailing whitespace")
	}
	work := strings.ReplaceAll(o.WorkingDirectory, "%", "%%")
	var b strings.Builder
	fmt.Fprintf(&b, "[Unit]\nDescription=codex-model-router (%s)\n\n[Service]\nType=exec\nExecStart=:%s\nWorkingDirectory=%s\nRestart=on-failure\nRestartSec=%d\nStandardOutput=journal\nStandardError=journal\n", label, strings.Join(args, " "), work, o.ThrottleInterval)
	names := make([]string, 0, len(o.Env))
	for name := range o.Env {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := validEnvName(name); err != nil {
			return "", "", err
		}
		value, err := unitQuote(name + "=" + o.Env[name])
		if err != nil {
			return "", "", err
		}
		fmt.Fprintf(&b, "Environment=%s\n", value)
	}
	b.WriteString("\n[Install]\nWantedBy=default.target\n")
	return b.String(), filepath.Join(o.UnitDir, label+".service"), nil
}

func ownedUnit(data []byte, binary string) bool {
	quoted, err := unitQuote(binary)
	if err != nil {
		return false
	}
	prefix := "ExecStart=:" + quoted + " \"serve\" \"--config\" "
	count := 0
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "ExecStart=") {
			if !strings.HasPrefix(line, prefix) {
				return false
			}
			count++
		}
	}
	return count == 1
}

func (m *Manager) systemdInstall(ctx context.Context, dryRun, force bool) (Report, error) {
	rendered, path, err := m.systemdPreview()
	report := Report{UnitPath: path}
	if err != nil {
		return report, err
	}
	if err := m.checkBinary(m.Options.BinaryPath); err != nil {
		return report, err
	}
	if info, err := os.Stat(m.Options.ConfigPath); err != nil || info.IsDir() {
		return report, fmt.Errorf("a configuration file is required at %s", m.Options.ConfigPath)
	}
	old, err := os.ReadFile(path)
	if err == nil && !ownedUnit(old, m.Options.BinaryPath) && !force {
		return report, fmt.Errorf("%s is not managed by this tool; use --force to replace it", path)
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return report, err
	}
	unit := filepath.Base(path)
	report.Steps = []Step{{Action: "write unit", Detail: path}, {Action: "systemctl --user daemon-reload"}, {Action: "systemctl --user enable", Detail: unit}, {Action: "systemctl --user restart", Detail: unit}}
	if dryRun {
		report.Notes = append(report.Notes, "dry run: nothing was written or loaded")
		return report, nil
	}
	// Refuse an unavailable user manager before modifying installation files.
	if _, err := m.runner().Run(ctx, "systemctl", "--user", "show-environment"); err != nil {
		return report, fmt.Errorf("systemd user manager is unavailable: %w", err)
	}
	mode := os.FileMode(0o644)
	if len(m.Options.Env) > 0 {
		mode = 0o600
	}
	if err := m.writeFileFunc()(path, []byte(rendered), mode); err != nil {
		return report, err
	}
	for _, args := range [][]string{{"--user", "daemon-reload"}, {"--user", "enable", unit}, {"--user", "restart", unit}} {
		if _, err := m.runner().Run(ctx, "systemctl", args...); err != nil {
			return report, fmt.Errorf("unit retained at %s after service operation failed: %w", path, err)
		}
	}
	report.Notes = append(report.Notes, "starts with the user manager; login-free startup requires separately configured lingering", "logs: journalctl --user -u "+unit)
	return report, nil
}

func (m *Manager) systemdUninstall(ctx context.Context, dryRun, force bool) (Report, error) {
	_, path, err := m.systemdPreview()
	report := Report{UnitPath: path}
	if err != nil {
		return report, err
	}
	old, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		report.Notes = append(report.Notes, "not installed: no unit at "+path)
		return report, nil
	}
	if err != nil {
		return report, err
	}
	if !ownedUnit(old, m.Options.BinaryPath) && !force {
		return report, fmt.Errorf("%s is not managed by this tool; not removing", path)
	}
	unit := filepath.Base(path)
	report.Steps = []Step{{Action: "systemctl --user disable --now", Detail: unit}, {Action: "remove unit", Detail: path}, {Action: "systemctl --user daemon-reload"}}
	if dryRun {
		report.Notes = append(report.Notes, "dry run: nothing was removed")
		return report, nil
	}
	if _, err := m.runner().Run(ctx, "systemctl", "--user", "disable", "--now", unit); err != nil {
		return report, fmt.Errorf("unit retained at %s: %w", path, err)
	}
	if err := m.removeFileFunc()(path); err != nil {
		return report, err
	}
	if _, err := m.runner().Run(ctx, "systemctl", "--user", "daemon-reload"); err != nil {
		return report, err
	}
	report.Notes = append(report.Notes, "binary, configuration, catalog, and journal were left untouched")
	return report, nil
}

func (m *Manager) systemdStatus(ctx context.Context) (Report, error) {
	_, path, err := m.systemdPreview()
	report := Report{UnitPath: path}
	if err != nil {
		return report, err
	}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		report.Notes = append(report.Notes, "not installed: no unit at "+path)
	} else if err != nil {
		return report, err
	}
	unit := filepath.Base(path)
	output, err := m.runner().Run(ctx, "systemctl", "--user", "show", unit, "--property=LoadState,ActiveState,SubState,MainPID,ExecMainStatus,UnitFileState")
	if err != nil {
		report.Notes = append(report.Notes, "systemd state unavailable: "+firstLine(err))
	} else {
		report.Steps = append(report.Steps, Step{Action: "systemd state", Detail: strings.TrimSpace(string(output))})
	}
	report.Notes = append(report.Notes, "logs: journalctl --user -u "+unit)
	if m.HealthProbe != nil {
		health, err := m.HealthProbe(ctx)
		if err != nil {
			report.Notes = append(report.Notes, "health probe failed: "+err.Error())
		} else {
			report.Health = health
			report.Steps = append(report.Steps, Step{Action: "health", Detail: summarizeHealth(health)})
		}
	}
	return report, nil
}
