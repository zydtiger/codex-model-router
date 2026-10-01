package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestPackageReleaseRefusesLightweightTagBeforeWritingArtifacts(t *testing.T) {
	// Git hooks export repository context that must not reach fixture commands.
	hookDirectory := t.TempDir()
	t.Setenv("GIT_DIR", filepath.Join(hookDirectory, "repository.git"))
	t.Setenv("GIT_WORK_TREE", hookDirectory)
	t.Setenv("GIT_INDEX_FILE", filepath.Join(hookDirectory, "index"))
	directory := t.TempDir()
	runGit(t, directory, "init", "-q")
	runGit(t, directory, "config", "user.email", "release-test@example.invalid")
	runGit(t, directory, "config", "user.name", "Release Test")
	runGit(t, directory, "commit", "--allow-empty", "-qm", "test release tag")
	runGit(t, directory, "tag", "v0.1.0")

	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate package release test source")
	}
	script := filepath.Join(filepath.Dir(source), "..", "..", "scripts", "package-release.sh")
	outputDir := filepath.Join(directory, "dist")
	command := exec.Command("bash", script, outputDir)
	command.Dir = directory
	command.Env = isolatedGitEnvironment()
	output, err := command.CombinedOutput()
	if err == nil {
		t.Fatal("package release accepted a lightweight tag")
	}
	if !strings.Contains(string(output), "release tag v0.1.0 must be annotated") {
		t.Fatalf("package release output = %q", output)
	}
	if _, err := os.Stat(outputDir); !os.IsNotExist(err) {
		t.Fatalf("lightweight tag created release output: %v", err)
	}
}

func TestPackageReleaseRefusesExistingChecksumBeforeBuilding(t *testing.T) {
	directory := t.TempDir()
	runGit(t, directory, "init", "-q")
	runGit(t, directory, "config", "user.email", "release-test@example.invalid")
	runGit(t, directory, "config", "user.name", "Release Test")
	if err := os.WriteFile(filepath.Join(directory, "LICENSE"), []byte("test license\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "go.mod"), []byte("module example.test/router\n\ngo 1.27.0\ntoolchain go1.27.1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, directory, "add", "LICENSE", "go.mod")
	runGit(t, directory, "commit", "-qm", "test release checksum")
	runGit(t, directory, "tag", "-a", "v0.1.0", "-m", "test release tag")

	outputDir := t.TempDir()
	checksum := filepath.Join(outputDir, "SHA256SUMS")
	original := []byte("existing checksum bytes\n")
	if err := os.WriteFile(checksum, original, 0o600); err != nil {
		t.Fatal(err)
	}
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate package release test source")
	}
	command := exec.Command("bash", filepath.Join(filepath.Dir(source), "..", "..", "scripts", "package-release.sh"), outputDir)
	command.Dir = directory
	command.Env = isolatedGitEnvironment()
	output, err := command.CombinedOutput()
	if err == nil {
		t.Fatal("package release replaced an existing checksum")
	}
	if !strings.Contains(string(output), "refusing to replace existing checksum artifact") {
		t.Fatalf("package release output = %q", output)
	}
	after, err := os.ReadFile(checksum)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(original) {
		t.Fatalf("checksum changed from %q to %q", original, after)
	}
	entries, err := os.ReadDir(outputDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "SHA256SUMS" {
		t.Fatalf("checksum rejection created artifacts: %v", entries)
	}
}

func TestPackageReleaseRefusesDanglingArchiveLinkBeforeBuilding(t *testing.T) {
	directory := t.TempDir()
	runGit(t, directory, "init", "-q")
	runGit(t, directory, "config", "user.email", "release-test@example.invalid")
	runGit(t, directory, "config", "user.name", "Release Test")
	if err := os.WriteFile(filepath.Join(directory, "LICENSE"), []byte("test license\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "go.mod"), []byte("module example.test/router\n\ngo 1.27.0\ntoolchain go1.27.1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, directory, "add", "LICENSE", "go.mod")
	runGit(t, directory, "commit", "-qm", "test release archive link")
	runGit(t, directory, "tag", "-a", "v0.1.0", "-m", "test release tag")

	outputDir := t.TempDir()
	archive := filepath.Join(outputDir, "codex-model-router_0.1.0_darwin_arm64.tar.gz")
	if err := os.Symlink("missing-archive.tar.gz", archive); err != nil {
		t.Fatal(err)
	}
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate package release test source")
	}
	command := exec.Command("bash", filepath.Join(filepath.Dir(source), "..", "..", "scripts", "package-release.sh"), outputDir)
	command.Dir = directory
	command.Env = isolatedGitEnvironment()
	output, err := command.CombinedOutput()
	if err == nil {
		t.Fatal("package release replaced a dangling archive link")
	}
	if !strings.Contains(string(output), "refusing to replace existing artifact") {
		t.Fatalf("package release output = %q", output)
	}
	if target, err := os.Readlink(archive); err != nil || target != "missing-archive.tar.gz" {
		t.Fatalf("archive link changed to %q: %v", target, err)
	}
	entries, err := os.ReadDir(outputDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != filepath.Base(archive) {
		t.Fatalf("archive link rejection created artifacts: %v", entries)
	}
}

func runGit(t *testing.T, directory string, args ...string) {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = directory
	command.Env = isolatedGitEnvironment()
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
}

func isolatedGitEnvironment() []string {
	var environment []string
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "GIT_") {
			environment = append(environment, entry)
		}
	}
	return environment
}
