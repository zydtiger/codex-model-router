package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func linuxOptions(t *testing.T) Options {
	o := testOptions(t)
	o.Platform = "linux"
	o.UnitDir = filepath.Join(filepath.Dir(o.BinaryPath), "systemd", "user")
	o.PlistDir = ""
	o.LogDir = ""
	return o
}

func TestSystemdRenderEscapesLiteralPathsAndEnvironment(t *testing.T) {
	o := linuxOptions(t)
	o.BinaryPath = "/tmp/space %n $HOME/route\"r"
	o.WorkingDirectory = "/tmp/work %n $HOME space"
	o.Env = map[string]string{"TOKEN": "literal %n $HOME \\ \""}
	m := &Manager{Options: o}
	rendered, resolved, err := m.Preview()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`ExecStart=:"/tmp/space %%n $HOME/route\"r" "serve" "--config"`, `Environment="TOKEN=literal %%n $HOME \\ \""`, "WorkingDirectory=/tmp/work %%n $HOME space", "StandardError=journal", "WantedBy=default.target"} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("missing %q in\n%s", want, rendered)
		}
	}
	if !strings.HasSuffix(resolved.UnitPath, ".service") {
		t.Fatal(resolved)
	}
	o.Env["TOKEN"] = "injected\nExecStart=/bin/false"
	if _, _, err := (&Manager{Options: o}).Preview(); err == nil {
		t.Fatal("accepted a newline")
	}
}

func TestSystemdLifecycleAndDryRun(t *testing.T) {
	o := linuxOptions(t)
	runner := &fakeRunner{}
	m := &Manager{Options: o, Runner: runner}
	_, r, err := m.Preview()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Install(context.Background(), true, false); err != nil {
		t.Fatal(err)
	}
	if len(runner.commands) != 0 {
		t.Fatal("dry run invoked systemctl")
	}
	if _, err := os.Stat(o.UnitDir); !os.IsNotExist(err) {
		t.Fatal("dry run wrote files")
	}
	if _, err := m.Install(context.Background(), false, false); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(r.UnitPath)
	if err != nil {
		t.Fatal(err)
	}
	if !ownedUnit(data, o.BinaryPath) {
		t.Fatal("installed unit is not recognized")
	}
	if got := strings.Join(runner.commands[len(runner.commands)-1], " "); got != "systemctl --user restart "+o.Label+".service" {
		t.Fatal(got)
	}
	if _, err := m.Install(context.Background(), false, false); err != nil {
		t.Fatalf("reinstall: %v", err)
	}
	report, err := m.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(report.Describe(), "journalctl --user") {
		t.Fatal(report.Describe())
	}
	if _, err := m.Uninstall(context.Background(), true, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(r.UnitPath); err != nil {
		t.Fatal("dry uninstall removed unit")
	}
	if _, err := m.Uninstall(context.Background(), false, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(r.UnitPath); !os.IsNotExist(err) {
		t.Fatal("unit remains")
	}
	if _, err := os.Stat(o.ConfigPath); err != nil {
		t.Fatal("config was removed")
	}
}

func TestSystemdFailuresPreserveRecoverableState(t *testing.T) {
	for _, stage := range []string{"show-environment", "daemon-reload", "restart", "disable"} {
		t.Run(stage, func(t *testing.T) {
			o := linuxOptions(t)
			runner := &fakeRunner{}
			m := &Manager{Options: o, Runner: runner}
			_, r, _ := m.Preview()
			if stage == "disable" {
				if _, err := m.Install(context.Background(), false, false); err != nil {
					t.Fatal(err)
				}
			}
			runner.results = map[string]error{stage: errors.New("injected failure")}
			var err error
			if stage == "disable" {
				_, err = m.Uninstall(context.Background(), false, false)
			} else {
				_, err = m.Install(context.Background(), false, false)
			}
			if err == nil {
				t.Fatal("failure was hidden")
			}
			_, statErr := os.Stat(r.UnitPath)
			if stage == "show-environment" {
				if !os.IsNotExist(statErr) {
					t.Fatal("wrote without a user manager")
				}
			} else if statErr != nil {
				t.Fatal("lost recoverable unit")
			}
		})
	}
}

func TestSystemdUnmanagedFileAndSecretPermissions(t *testing.T) {
	o := linuxOptions(t)
	o.Env = map[string]string{"TOKEN": "secret"}
	m := &Manager{Options: o, Runner: &fakeRunner{}}
	_, r, _ := m.Preview()
	if err := os.MkdirAll(o.UnitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(r.UnitPath, []byte("[Service]\nExecStart=/bin/true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Install(context.Background(), false, false); err == nil {
		t.Fatal("overwrote unmanaged unit")
	}
	if _, err := m.Uninstall(context.Background(), false, false); err == nil {
		t.Fatal("removed unmanaged unit")
	}
	if _, err := m.Install(context.Background(), false, true); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(r.UnitPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatal("secret environment is world readable")
	}
}
