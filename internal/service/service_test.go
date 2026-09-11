package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeRunner records the commands the lifecycle would run.
type fakeRunner struct {
	commands [][]string
	results  map[string]error
	outputs  map[string]string
}

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	command := append([]string{name}, args...)
	f.commands = append(f.commands, command)
	key := strings.Join(command, " ")
	for pattern, err := range f.results {
		if strings.Contains(key, pattern) {
			return []byte(f.outputs[key]), err
		}
	}
	return []byte(f.outputs[key]), nil
}

func (f *fakeRunner) called(prefix string) int {
	count := 0
	for _, command := range f.commands {
		if strings.HasPrefix(strings.Join(command, " "), prefix) {
			count++
		}
	}
	return count
}

func testOptions(t *testing.T) Options {
	t.Helper()
	directory := t.TempDir()
	binary := filepath.Join(directory, "codex-model-router")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(directory, "config.json")
	if err := os.WriteFile(configPath, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	return Options{
		Platform:   "darwin",
		Label:      "test.codex-model-router",
		BinaryPath: binary,
		ConfigPath: configPath,
		PlistDir:   filepath.Join(directory, "LaunchAgents"),
		LogDir:     filepath.Join(directory, "logs"),
		UID:        501,
	}
}

func TestRenderedAgentRunsServeWithTheConfig(t *testing.T) {
	options := testOptions(t)
	rendered, err := RenderLaunchAgent(options)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	document, err := parsePlist([]byte(rendered))
	if err != nil {
		t.Fatalf("the rendered plist does not parse: %v\n%s", err, rendered)
	}
	if label, _ := stringValue(document, "Label"); label != options.Label {
		t.Fatalf("Label = %q", label)
	}
	arguments, ok := stringSliceValue(document, "ProgramArguments")
	if !ok {
		t.Fatalf("ProgramArguments missing: %v", document)
	}
	if len(arguments) != 4 || arguments[0] != options.BinaryPath || arguments[1] != "serve" ||
		arguments[2] != "--config" || arguments[3] != options.ConfigPath {
		t.Fatalf("ProgramArguments = %v", arguments)
	}
	if runAtLoad, ok := document["RunAtLoad"].(bool); !ok || !runAtLoad {
		t.Fatal("RunAtLoad must be true so the router starts at login")
	}
	keepAlive, ok := document["KeepAlive"].(map[string]any)
	if !ok {
		t.Fatalf("KeepAlive = %v", document["KeepAlive"])
	}
	if successful, ok := keepAlive["SuccessfulExit"].(bool); !ok || successful {
		t.Fatal("KeepAlive.SuccessfulExit must be false: a clean exit is not a crash")
	}
	if crashed, ok := keepAlive["Crashed"].(bool); !ok || !crashed {
		t.Fatal("KeepAlive.Crashed must be true so a crash restarts the router")
	}
	if stdout, _ := stringValue(document, "StandardOutPath"); stdout != filepath.Join(options.LogDir, options.Label+".out.log") {
		t.Fatalf("StandardOutPath = %q", stdout)
	}
	if throttle, _ := stringValue(document, "ThrottleInterval"); throttle != "10" {
		t.Fatalf("ThrottleInterval = %q", throttle)
	}
}

func TestRenderEscapesAndRejectsUnsafeValues(t *testing.T) {
	options := testOptions(t)
	options.Label = ""
	options.BinaryPath = filepath.Join(t.TempDir(), "weird & <name>/codex-model-router")
	rendered, err := RenderLaunchAgent(options)
	if err != nil {
		t.Fatalf("a path with XML metacharacters must still render: %v", err)
	}
	document, err := parsePlist([]byte(rendered))
	if err != nil {
		t.Fatalf("escaped plist does not parse: %v", err)
	}
	arguments, _ := stringSliceValue(document, "ProgramArguments")
	if arguments[0] != options.BinaryPath {
		t.Fatalf("escaped path did not round trip: %q vs %q", arguments[0], options.BinaryPath)
	}
	if strings.Contains(rendered, "<string>weird & <name>") {
		t.Fatal("the path was written without escaping")
	}

	options = testOptions(t)
	options.Env = map[string]string{"SOME_KEY": "value</string><string>injected"}
	rendered, err = RenderLaunchAgent(options)
	if err != nil {
		t.Fatalf("render with env: %v", err)
	}
	document, err = parsePlist([]byte(rendered))
	if err != nil {
		t.Fatalf("escaped env plist does not parse: %v", err)
	}
	environment, ok := document["EnvironmentVariables"].(map[string]any)
	if !ok {
		t.Fatal("EnvironmentVariables missing")
	}
	if environment["SOME_KEY"] != "value</string><string>injected" {
		t.Fatalf("injected env value came back as %v", environment["SOME_KEY"])
	}
	// The escape must not create an extra array or dict entry.
	if strings.Count(rendered, "<string>") != strings.Count(rendered, "</string>") {
		t.Fatal("escaping produced unbalanced string elements")
	}

	options = testOptions(t)
	options.Env = map[string]string{"BAD": "control\x01character"}
	if _, err := RenderLaunchAgent(options); err == nil {
		t.Fatal("a control character in an environment value must be refused")
	}

	options = testOptions(t)
	options.Env = map[string]string{"not a name": "value"}
	if _, err := RenderLaunchAgent(options); err == nil {
		t.Fatal("an invalid environment variable name must be refused")
	}

	options = testOptions(t)
	options.Label = "bad label with spaces"
	if _, err := RenderLaunchAgent(options); err == nil {
		t.Fatal("an invalid label must be refused")
	}
}

func TestEnvBlockIsAbsentWithoutEnvironment(t *testing.T) {
	rendered, err := RenderLaunchAgent(testOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(rendered, "EnvironmentVariables") {
		t.Fatalf("no environment block should be emitted:\n%s", rendered)
	}
}

func TestOwnedDetectsThisInstallersFiles(t *testing.T) {
	options := testOptions(t)
	rendered, err := RenderLaunchAgent(options)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := options.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if !Owned([]byte(rendered), resolved.Label, resolved.BinaryPath) {
		t.Fatal("a plist this package rendered should be recognised as ours")
	}
	foreign := `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><dict>
<key>Label</key><string>com.example.other</string>
<key>ProgramArguments</key><array><string>/usr/bin/true</string></array>
</dict></plist>`
	if Owned([]byte(foreign), resolved.Label, resolved.BinaryPath) {
		t.Fatal("a foreign plist was treated as ours")
	}
	if Owned([]byte("not xml at all"), resolved.Label, resolved.BinaryPath) {
		t.Fatal("unparseable content must not be treated as ours")
	}
	// Same label but a different binary is a different installation.
	renderedElsewhere := strings.Replace(rendered, options.BinaryPath, "/elsewhere/codex-model-router", 1)
	if Owned([]byte(renderedElsewhere), resolved.Label, resolved.BinaryPath) {
		t.Fatal("a plist pointing at another binary must not be treated as ours")
	}
}

func TestInstallRunsLaunchctlInTheRightOrder(t *testing.T) {
	options := testOptions(t)
	runner := &fakeRunner{}
	manager := &Manager{Options: options, Runner: runner}

	report, err := manager.Install(context.Background(), false, false)
	if err != nil {
		t.Fatalf("install: %v\n%s", err, report.Describe())
	}
	if runner.called("sudo") != 0 {
		t.Fatalf("the installer must not use sudo: %v", runner.commands)
	}
	joined := describeCommands(runner.commands)
	for _, want := range []string{
		// launchctl's per-service verbs take one service-target, not a domain and a
		// label as two arguments.
		"launchctl bootout gui/501/test.codex-model-router",
		"launchctl bootstrap gui/501 " + filepath.Join(options.PlistDir, options.Label+".plist"),
		"launchctl kickstart -k gui/501/test.codex-model-router",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing launchctl call %q in:\n%s", want, joined)
		}
	}
	// bootout must come before bootstrap: bootstrap refuses a loaded label.
	bootout, bootstrap := indexOfCommand(runner.commands, "bootout"), indexOfCommand(runner.commands, "bootstrap")
	if bootout < 0 || bootstrap < 0 || bootout > bootstrap {
		t.Fatalf("command order is wrong:\n%s", joined)
	}

	plistPath := filepath.Join(options.PlistDir, options.Label+".plist")
	content, err := os.ReadFile(plistPath)
	if err != nil {
		t.Fatalf("the plist was not written: %v", err)
	}
	if !Owned(content, options.Label, options.BinaryPath) {
		t.Fatal("the written plist is not recognised as ours")
	}
	if info, err := os.Stat(plistPath); err != nil {
		t.Fatal(err)
	} else if info.Mode().Perm() != 0o644 {
		t.Fatalf("without an environment block the plist should be 0644, got %o", info.Mode().Perm())
	}
	// The log directory is created so launchd can open the log files.
	if _, err := os.Stat(options.LogDir); err != nil {
		t.Fatalf("the log directory was not created: %v", err)
	}
	if len(report.Steps) == 0 {
		t.Fatal("the report has no steps")
	}
}

func TestInstallWithEnvironmentRestrictsPermissions(t *testing.T) {
	options := testOptions(t)
	options.Env = map[string]string{"SGLANG_API_KEY": "local-server-key"}
	manager := &Manager{Options: options, Runner: &fakeRunner{}}
	if _, err := manager.Install(context.Background(), false, false); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(options.PlistDir, options.Label+".plist"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("a plist carrying a key must be 0600, got %o", info.Mode().Perm())
	}
}

func TestInstallDryRunWritesNothing(t *testing.T) {
	options := testOptions(t)
	runner := &fakeRunner{}
	report, err := (&Manager{Options: options, Runner: runner}).Install(context.Background(), true, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(runner.commands) != 0 {
		t.Fatalf("dry run ran commands: %v", runner.commands)
	}
	if _, err := os.Stat(filepath.Join(options.PlistDir, options.Label+".plist")); !os.IsNotExist(err) {
		t.Fatal("dry run wrote the plist")
	}
	if _, err := os.Stat(options.PlistDir); !os.IsNotExist(err) {
		t.Fatal("dry run created the LaunchAgents directory")
	}
	if !strings.Contains(report.Describe(), "dry run") {
		t.Fatalf("the report should say it was a dry run:\n%s", report.Describe())
	}
}

func TestInstallRefusesForeignPlist(t *testing.T) {
	options := testOptions(t)
	if err := os.MkdirAll(options.PlistDir, 0o755); err != nil {
		t.Fatal(err)
	}
	foreign := []byte("<?xml version=\"1.0\"?><plist><dict><key>Label</key><string>someone.else</string></dict></plist>")
	plistPath := filepath.Join(options.PlistDir, options.Label+".plist")
	if err := os.WriteFile(plistPath, foreign, 0o644); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{}
	if _, err := (&Manager{Options: options, Runner: runner}).Install(context.Background(), false, false); err == nil {
		t.Fatal("a foreign plist was overwritten")
	}
	onDisk, err := os.ReadFile(plistPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(onDisk) != string(foreign) {
		t.Fatal("the foreign plist changed")
	}
	if len(runner.commands) != 0 {
		t.Fatalf("launchctl ran after a refused overwrite: %v", runner.commands)
	}
	if _, err := (&Manager{Options: options, Runner: &fakeRunner{}}).Install(context.Background(), false, true); err != nil {
		t.Fatalf("--force should allow the replacement: %v", err)
	}
}

func TestInstallChecksTheBinaryAndConfig(t *testing.T) {
	options := testOptions(t)
	options.BinaryPath = filepath.Join(t.TempDir(), "missing-binary")
	if _, err := (&Manager{Options: options, Runner: &fakeRunner{}}).Install(context.Background(), false, false); err == nil {
		t.Fatal("a missing binary should stop the install")
	}

	options = testOptions(t)
	notExecutable := filepath.Join(t.TempDir(), "codex-model-router")
	if err := os.WriteFile(notExecutable, []byte("text"), 0o644); err != nil {
		t.Fatal(err)
	}
	options.BinaryPath = notExecutable
	if _, err := (&Manager{Options: options, Runner: &fakeRunner{}}).Install(context.Background(), false, false); err == nil {
		t.Fatal("a non-executable binary should stop the install")
	}

	options = testOptions(t)
	options.ConfigPath = filepath.Join(t.TempDir(), "absent.json")
	if _, err := (&Manager{Options: options, Runner: &fakeRunner{}}).Install(context.Background(), false, false); err == nil {
		t.Fatal("a missing config should stop the install")
	}
}

func TestInstallReportsBootstrapFailure(t *testing.T) {
	options := testOptions(t)
	runner := &fakeRunner{results: map[string]error{"bootstrap": errors.New("Bootstrap failed: 5: Input/output error")}}
	_, err := (&Manager{Options: options, Runner: runner}).Install(context.Background(), false, false)
	if err == nil {
		t.Fatal("a bootstrap failure must be reported as an error")
	}
	if !strings.Contains(err.Error(), "bootstrap failed") {
		t.Fatalf("error = %v", err)
	}
	// The plist stays so the user can retry or uninstall cleanly.
	if _, statErr := os.Stat(filepath.Join(options.PlistDir, options.Label+".plist")); statErr != nil {
		t.Fatalf("the plist should remain after a failed bootstrap: %v", statErr)
	}
}

func TestUninstallKeepsUserData(t *testing.T) {
	options := testOptions(t)
	runner := &fakeRunner{}
	if _, err := (&Manager{Options: options, Runner: runner}).Install(context.Background(), false, false); err != nil {
		t.Fatal(err)
	}
	runner.commands = nil

	report, err := (&Manager{Options: options, Runner: runner}).Uninstall(context.Background(), false, false)
	if err != nil {
		t.Fatal(err)
	}
	joined := describeCommands(runner.commands)
	if !strings.Contains(joined, "launchctl bootout gui/501/test.codex-model-router") {
		t.Fatalf("uninstall did not bootout:\n%s", joined)
	}
	plistPath := filepath.Join(options.PlistDir, options.Label+".plist")
	if _, err := os.Stat(plistPath); !os.IsNotExist(err) {
		t.Fatal("the plist should be removed")
	}
	// The router configuration is a user file and must survive.
	if _, err := os.Stat(options.ConfigPath); err != nil {
		t.Fatalf("uninstall removed the user's configuration: %v", err)
	}
	if !strings.Contains(report.Describe(), "left untouched") {
		t.Fatalf("the report should say what was kept:\n%s", report.Describe())
	}
}

func TestUninstallIsIdempotentAndRefusesForeignFiles(t *testing.T) {
	options := testOptions(t)
	runner := &fakeRunner{}
	report, err := (&Manager{Options: options, Runner: runner}).Uninstall(context.Background(), false, false)
	if err != nil {
		t.Fatalf("uninstalling nothing should succeed: %v", err)
	}
	if !strings.Contains(report.Describe(), "no LaunchAgent") {
		t.Fatalf("report = %s", report.Describe())
	}
	if len(runner.commands) != 0 {
		t.Fatalf("nothing is installed, so launchctl should not run: %v", runner.commands)
	}

	if err := os.MkdirAll(options.PlistDir, 0o755); err != nil {
		t.Fatal(err)
	}
	plistPath := filepath.Join(options.PlistDir, options.Label+".plist")
	if err := os.WriteFile(plistPath, []byte("<plist><dict><key>Label</key><string>someone.else</string></dict></plist>"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := (&Manager{Options: options, Runner: &fakeRunner{}}).Uninstall(context.Background(), false, false); err == nil {
		t.Fatal("a foreign plist should not be removed")
	}
	if _, err := os.Stat(plistPath); err != nil {
		t.Fatal("the foreign plist was removed")
	}
}

func TestUninstallReportsARealBootoutFailure(t *testing.T) {
	options := testOptions(t)
	if _, err := (&Manager{Options: options, Runner: &fakeRunner{}}).Install(context.Background(), false, false); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{results: map[string]error{"bootout": errors.New("Boot-out failed: 5: Input/output error")}}
	_, err := (&Manager{Options: options, Runner: runner}).Uninstall(context.Background(), false, false)
	if err == nil {
		t.Fatal("a failed bootout must be reported as a failure, not as a successful uninstall")
	}
	if !strings.Contains(err.Error(), "was kept so the agent can be retried") {
		t.Fatalf("error = %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(options.PlistDir, options.Label+".plist")); statErr != nil {
		t.Fatalf("the plist should be kept when bootout failed: %v", statErr)
	}
	// Removing the file is the last step, so a bootout failure means no removal ran.
	if runner.called("remove") != 0 {
		t.Fatalf("removal should not run after a failed bootout: %v", runner.commands)
	}
}

func TestUninstallRemovesAPlawithNoLoadedAgent(t *testing.T) {
	options := testOptions(t)
	if _, err := (&Manager{Options: options, Runner: &fakeRunner{}}).Install(context.Background(), false, false); err != nil {
		t.Fatal(err)
	}
	// "No such process" is how launchctl reports a label that is not loaded, which is
	// the state after a crash-and-reap or an interrupted install.
	runner := &fakeRunner{results: map[string]error{"bootout": errors.New("Boot-out failed: 3: No such process")}}
	report, err := (&Manager{Options: options, Runner: runner}).Uninstall(context.Background(), false, false)
	if err != nil {
		t.Fatalf("an agent that was not loaded should not stop the cleanup: %v", err)
	}
	if !strings.Contains(report.Describe(), "no agent was loaded") {
		t.Fatalf("report = %s", report.Describe())
	}
	if _, err := os.Stat(filepath.Join(options.PlistDir, options.Label+".plist")); !os.IsNotExist(err) {
		t.Fatal("the plist should be removed when nothing was loaded")
	}
}

func TestInstallStopsWhenBootoutFails(t *testing.T) {
	options := testOptions(t)
	runner := &fakeRunner{results: map[string]error{"bootout": errors.New("Boot-out failed: 5: Input/output error")}}
	_, err := (&Manager{Options: options, Runner: runner}).Install(context.Background(), false, false)
	if err == nil {
		t.Fatal("a real bootout failure should stop the install")
	}
	if _, err := os.Stat(filepath.Join(options.PlistDir, options.Label+".plist")); !os.IsNotExist(err) {
		t.Fatal("the install wrote the plist even though bootout failed")
	}
	if runner.called("bootstrap") != 0 {
		t.Fatalf("bootstrap should not run after a failed bootout: %v", runner.commands)
	}
}

func TestServiceTargetsMatchLaunchctlSyntax(t *testing.T) {
	options := testOptions(t)
	resolved, err := options.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	// launchctl help: kickstart [-k] <service-target>; bootout <domain-target> [paths]
	// | <service-target>; print <service-target>.
	if resolved.ServiceTarget() != "gui/501/test.codex-model-router" {
		t.Fatalf("ServiceTarget = %q", resolved.ServiceTarget())
	}
	if resolved.Domain() != "gui/501" {
		t.Fatalf("Domain = %q", resolved.Domain())
	}
}

func TestUninstallDryRun(t *testing.T) {
	options := testOptions(t)
	runner := &fakeRunner{}
	if _, err := (&Manager{Options: options, Runner: &fakeRunner{}}).Install(context.Background(), false, false); err != nil {
		t.Fatal(err)
	}
	runner.commands = nil
	report, err := (&Manager{Options: options, Runner: runner}).Uninstall(context.Background(), true, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(runner.commands) != 0 {
		t.Fatalf("dry run ran launchctl: %v", runner.commands)
	}
	if _, err := os.Stat(filepath.Join(options.PlistDir, options.Label+".plist")); err != nil {
		t.Fatal("dry run removed the plist")
	}
	if !strings.Contains(report.Describe(), "dry run") {
		t.Fatalf("report = %s", report.Describe())
	}
}

func TestStatusReportsLaunchdAndHealth(t *testing.T) {
	options := testOptions(t)
	runner := &fakeRunner{outputs: map[string]string{
		"launchctl print gui/501/test.codex-model-router": "gui/501/test.codex-model-router = {\n\tstate = running\n\tpid = 4321\n\trun type = Manual\n\tlast exit code = (never exited)\n}\n",
	}}
	if _, err := (&Manager{Options: options, Runner: &fakeRunner{}}).Install(context.Background(), false, false); err != nil {
		t.Fatal(err)
	}
	healthCalled := 0
	manager := &Manager{Options: options, Runner: runner, HealthProbe: func(context.Context) (map[string]any, error) {
		healthCalled++
		return map[string]any{"ok": true, "version": "0.1.0", "requests": float64(3)}, nil
	}}
	report, err := manager.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if healthCalled != 1 {
		t.Fatalf("health probes = %d", healthCalled)
	}
	described := report.Describe()
	for _, want := range []string{"state=running", "pid=4321", "managed by this tool", "ok=true"} {
		if !strings.Contains(described, want) {
			t.Fatalf("status output is missing %q:\n%s", want, described)
		}
	}

	// A loaded agent with an unreachable router must be visible as such.
	manager = &Manager{Options: options, Runner: runner, HealthProbe: func(context.Context) (map[string]any, error) {
		return nil, errors.New("connection refused")
	}}
	report, err = manager.Status(context.Background())
	if err != nil {
		t.Fatalf("a failing health probe is a note, not a command failure: %v", err)
	}
	if !strings.Contains(report.Describe(), "health probe failed") {
		t.Fatalf("report = %s", report.Describe())
	}
}

func TestStatusWithoutInstallation(t *testing.T) {
	options := testOptions(t)
	runner := &fakeRunner{results: map[string]error{"print": errors.New("Could not find service in domain")}}
	report, err := (&Manager{Options: options, Runner: runner, HealthProbe: func(context.Context) (map[string]any, error) {
		return nil, errors.New("no router is listening")
	}}).Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	described := report.Describe()
	if !strings.Contains(described, "not installed") || !strings.Contains(described, "not loaded") {
		t.Fatalf("report = %s", described)
	}
}

func TestResolveValidatesInput(t *testing.T) {
	options := testOptions(t)
	resolved, err := options.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Domain() != "gui/501" {
		t.Fatalf("Domain = %q", resolved.Domain())
	}
	if resolved.PlistPath != filepath.Join(options.PlistDir, options.Label+".plist") {
		t.Fatalf("PlistPath = %q", resolved.PlistPath)
	}
	if resolved.StdErrPath != filepath.Join(options.LogDir, options.Label+".err.log") {
		t.Fatalf("StdErrPath = %q", resolved.StdErrPath)
	}

	cases := map[string]func(*Options){
		"binary":     func(o *Options) { o.BinaryPath = "" },
		"config":     func(o *Options) { o.ConfigPath = "" },
		"plist dir":  func(o *Options) { o.PlistDir = "" },
		"log dir":    func(o *Options) { o.LogDir = "" },
		"throttle":   func(o *Options) { o.ThrottleInterval = -1 },
		"uid":        func(o *Options) { o.UID = -1 },
		"bad label":  func(o *Options) { o.Label = "a..b" },
		"empty path": func(o *Options) { o.BinaryPath = "   " },
	}
	for name, mutate := range cases {
		broken := options
		mutate(&broken)
		if _, err := broken.Resolve(); err == nil {
			t.Fatalf("%s should be refused", name)
		}
	}
}

func TestDefaultPathsAreUnderHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory available")
	}
	plistDir, err := DefaultPlistDir()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(plistDir, filepath.Join(home, "Library", "LaunchAgents")) {
		t.Fatalf("DefaultPlistDir = %q", plistDir)
	}
	logDir, err := DefaultLogDir(DefaultLabel)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(logDir, filepath.Join(home, "Library", "Logs")) {
		t.Fatalf("DefaultLogDir = %q", logDir)
	}
	binary, err := DefaultBinaryPath()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(binary, home) || filepath.Base(binary) != "codex-model-router" {
		t.Fatalf("DefaultBinaryPath = %q", binary)
	}
	// The stable path must not be inside a checkout, so upgrading a clone cannot
	// break a running agent.
	if cwd, err := os.Getwd(); err == nil && strings.HasPrefix(binary, cwd) {
		t.Fatalf("the default install path is inside the working tree: %q", binary)
	}
}

func TestExecRunnerReportsCommandFailure(t *testing.T) {
	output, err := ExecRunner{}.Run(context.Background(), "sh", "-c", "echo boom; exit 3")
	if err == nil {
		t.Fatal("a failing command should return an error")
	}
	if !strings.Contains(err.Error(), "boom") || !strings.Contains(err.Error(), "exit status 3") {
		t.Fatalf("error = %v", err)
	}
	if !strings.Contains(string(output), "boom") {
		t.Fatalf("output = %s", output)
	}
	runner := ExecRunner{}
	if _, err := runner.Run(context.Background(), "definitely-not-a-command-xyz"); err == nil {
		t.Fatal("a missing binary should return an error")
	}
}

func TestSummarizeLaunchdOutputIgnoresNoise(t *testing.T) {
	summary := summarizeLaunchdOutput("gui/501/x = {\n\tstate = running\n\tpid = 7\n\tprogram = /bin/x\n\trandom = ignored\n}\n")
	if summary != "pid=7 state=running" {
		t.Fatalf("summary = %q", summary)
	}
	if got := summarizeLaunchdOutput("nothing useful"); got != "loaded" {
		t.Fatalf("summary = %q", got)
	}
}

func TestNotLoadedMessageCoversLaunchctlWording(t *testing.T) {
	for _, message := range []string{
		"Could not find service \"x\" in domain for user",
		"No such process",
		"Boot-out failed: 3: No such process",
		"Could not find service \"x\" in domain",
		"The service is not found in the requested domain",
	} {
		if !notLoadedMessage(errors.New(message)) {
			t.Fatalf("%q should read as not-loaded", message)
		}
	}
	for _, message := range []string{
		"Bootstrap failed: 5: Input/output error",
		"Boot-out failed: 5: Input/output error",
		"launchctl: command not found",
		"5: the file already exists",
	} {
		if notLoadedMessage(errors.New(message)) {
			t.Fatalf("%q is a real failure, not a not-loaded state", message)
		}
	}
}

func TestSafeErrorMessageKeepsKnownFields(t *testing.T) {
	if got := firstLine(errors.New("line one\nline two")); got != "line one" {
		t.Fatalf("firstLine = %q", got)
	}
	if !notLoadedMessage(errors.New("Could not find service \"x\" in domain for user")) {
		t.Fatal("a not-loaded launchctl error should be recognised")
	}
	if notLoadedMessage(errors.New("Bootstrap failed: 5")) {
		t.Fatal("a bootstrap failure is not a not-loaded error")
	}
}

func describeCommands(commands [][]string) string {
	lines := make([]string, 0, len(commands))
	for _, command := range commands {
		lines = append(lines, strings.Join(command, " "))
	}
	return strings.Join(lines, "\n")
}

func indexOfCommand(commands [][]string, needle string) int {
	for index, command := range commands {
		if strings.Contains(strings.Join(command, " "), needle) {
			return index
		}
	}
	return -1
}

type bootstrapRunner struct {
	attempts int
	failures int
	failure  error
}

func (r *bootstrapRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	if name == "launchctl" && args[0] == "bootstrap" {
		r.attempts++
		if r.attempts <= r.failures {
			return nil, r.failure
		}
	}
	return nil, nil
}

func TestBootstrapRetriesTransientEIO(t *testing.T) {
	r := &bootstrapRunner{failures: 1, failure: errors.New("Bootstrap failed: 5: Input/output error")}
	m := &Manager{Options: testOptions(t), Runner: r}
	if _, err := m.Install(context.Background(), false, false); err != nil {
		t.Fatal(err)
	}
	if r.attempts != 2 {
		t.Fatalf("bootstrap attempts=%d", r.attempts)
	}
}

func TestBootstrapDoesNotRetryOtherFailures(t *testing.T) {
	r := &bootstrapRunner{failures: 10, failure: errors.New("Bootstrap failed: 1: Operation not permitted")}
	m := &Manager{Options: testOptions(t), Runner: r}
	if _, err := m.Install(context.Background(), false, false); err == nil {
		t.Fatal("failure hidden")
	}
	if r.attempts != 1 {
		t.Fatalf("bootstrap attempts=%d", r.attempts)
	}
}

func TestBootstrapRetryIsBoundedAndCancelable(t *testing.T) {
	r := &bootstrapRunner{failures: 10, failure: errors.New("Bootstrap failed: 5: Input/output error")}
	m := &Manager{Options: testOptions(t), Runner: r}
	resolved, err := m.resolved()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := m.bootstrap(ctx, resolved); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if r.attempts != 0 {
		t.Fatal("canceled context invoked launchctl")
	}
	if err := m.bootstrap(context.Background(), resolved); err == nil {
		t.Fatal("persistent failure hidden")
	}
	if r.attempts != 5 {
		t.Fatalf("bootstrap attempts=%d", r.attempts)
	}
}
