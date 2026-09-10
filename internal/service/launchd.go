// Package service renders and manages the macOS LaunchAgent that keeps the
// router running. The template and the lifecycle logic live in this repository so
// that deployment is reproducible from a checkout.
package service

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// DefaultLabel is the LaunchAgent label. It follows the repository's module path
// so it cannot collide with another vendor's agent.
const DefaultLabel = "com.github.zydtiger.codex-model-router"

// Template is the LaunchAgent plist. Every placeholder value is escaped before it
// reaches the template, so a path or environment value cannot inject XML.
const Template = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>{{Label}}</string>
	<key>ProgramArguments</key>
	<array>
{{ProgramArguments}}	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<dict>
		<key>SuccessfulExit</key>
		<false/>
		<key>Crashed</key>
		<true/>
	</dict>
	<key>ThrottleInterval</key>
	<integer>{{ThrottleInterval}}</integer>
	<key>WorkingDirectory</key>
	<string>{{WorkingDirectory}}</string>
	<key>StandardOutPath</key>
	<string>{{StdOutPath}}</string>
	<key>StandardErrorPath</key>
	<string>{{StdErrPath}}</string>
{{EnvironmentBlock}}</dict>
</plist>
`

// Options describes one managed LaunchAgent. Every path is configurable, which is
// what makes the lifecycle testable without touching a real machine.
type Options struct {
	// Label is the launchd label. Empty selects DefaultLabel.
	Label string
	// BinaryPath is the stable path the executable is installed to. The agent
	// runs this path, never a path inside a checkout.
	BinaryPath string
	// ConfigPath is passed to "serve --config".
	ConfigPath string
	// PlistDir is the LaunchAgents directory.
	PlistDir string
	// LogDir receives the agent's stdout and stderr files.
	LogDir string
	// WorkingDirectory for the agent process.
	WorkingDirectory string
	// Env is extra environment for the agent, for example an API key variable
	// that a login session would not otherwise provide.
	Env map[string]string
	// ThrottleInterval seconds between restart attempts. Zero selects 10.
	ThrottleInterval int
	// UID for the gui/<uid> launchd domain. Zero selects the current user.
	UID int
	// ExtraArgs are appended to "serve --config <path>".
	ExtraArgs []string
}

// Resolved is Options with defaults applied.
type Resolved struct {
	Options
	Label             string
	PlistPath         string
	StdOutPath        string
	StdErrPath        string
	WorkingDirectory2 string
}

// Domain is the launchd domain for a user agent.
func (r Resolved) Domain() string { return "gui/" + strconv.Itoa(r.UID) }

// ServiceTarget is the single argument launchctl's per-service verbs accept.
// bootout, kickstart, and print take one service-target of this form; passing a
// domain and a label as two arguments is a usage error, not a missing service.
func (r Resolved) ServiceTarget() string { return r.Domain() + "/" + r.Label }

// Resolve fills in defaults and derives the derived paths.
func (o Options) Resolve() (Resolved, error) {
	resolved := Resolved{Options: o}
	resolved.Label = strings.TrimSpace(o.Label)
	if resolved.Label == "" {
		resolved.Label = DefaultLabel
	}
	if err := validLabel(resolved.Label); err != nil {
		return Resolved{}, err
	}
	if strings.TrimSpace(o.BinaryPath) == "" {
		return Resolved{}, errors.New("a binary path is required")
	}
	if strings.TrimSpace(o.ConfigPath) == "" {
		return Resolved{}, errors.New("a config path is required")
	}
	if strings.TrimSpace(o.PlistDir) == "" {
		return Resolved{}, errors.New("a LaunchAgents directory is required")
	}
	if strings.TrimSpace(o.LogDir) == "" {
		return Resolved{}, errors.New("a log directory is required")
	}
	if o.ThrottleInterval < 0 {
		return Resolved{}, errors.New("throttle interval must not be negative")
	}
	if o.ThrottleInterval == 0 {
		resolved.ThrottleInterval = 10
	} else {
		resolved.ThrottleInterval = o.ThrottleInterval
	}
	if o.UID < 0 {
		return Resolved{}, errors.New("uid must not be negative")
	}
	home, err := os.UserHomeDir()
	if err != nil && strings.TrimSpace(o.WorkingDirectory) == "" {
		return Resolved{}, fmt.Errorf("resolve home directory: %w", err)
	}
	resolved.WorkingDirectory2 = strings.TrimSpace(o.WorkingDirectory)
	if resolved.WorkingDirectory2 == "" {
		resolved.WorkingDirectory2 = home
	}
	resolved.PlistPath = filepath.Join(o.PlistDir, resolved.Label+".plist")
	resolved.StdOutPath = filepath.Join(o.LogDir, resolved.Label+".out.log")
	resolved.StdErrPath = filepath.Join(o.LogDir, resolved.Label+".err.log")
	return resolved, nil
}

func validLabel(label string) error {
	if label == "" {
		return errors.New("label is empty")
	}
	for _, part := range strings.Split(label, ".") {
		if part == "" {
			return fmt.Errorf("label %q has an empty component", label)
		}
		for _, character := range part {
			switch {
			case character >= 'a' && character <= 'z', character >= 'A' && character <= 'Z', character >= '0' && character <= '9':
			case character == '-' || character == '_':
			default:
				return fmt.Errorf("label %q may only contain letters, digits, dots, dashes, and underscores", label)
			}
		}
	}
	return nil
}

// RenderLaunchAgent produces the plist document for the agent.
func RenderLaunchAgent(options Options) (string, error) {
	resolved, err := options.Resolve()
	if err != nil {
		return "", err
	}
	arguments := []string{resolved.BinaryPath, "serve", "--config", resolved.ConfigPath}
	arguments = append(arguments, resolved.ExtraArgs...)

	var program strings.Builder
	for _, argument := range arguments {
		if err := safeValue("program argument", argument); err != nil {
			return "", err
		}
		fmt.Fprintf(&program, "\t\t<string>%s</string>\n", escape(argument))
	}

	environment := ""
	if len(resolved.Env) > 0 {
		names := make([]string, 0, len(resolved.Env))
		for name := range resolved.Env {
			names = append(names, name)
		}
		sort.Strings(names)
		var builder strings.Builder
		builder.WriteString("\t<key>EnvironmentVariables</key>\n\t<dict>\n")
		for _, name := range names {
			if err := validEnvName(name); err != nil {
				return "", err
			}
			value := resolved.Env[name]
			if err := safeValue("environment value", value); err != nil {
				return "", err
			}
			fmt.Fprintf(&builder, "\t\t<key>%s</key>\n\t\t<string>%s</string>\n", escape(name), escape(value))
		}
		builder.WriteString("\t</dict>\n")
		environment = builder.String()
	}

	rendered := Template
	for _, replacement := range []struct{ key, value string }{
		{"{{Label}}", escape(resolved.Label)},
		{"{{ProgramArguments}}", program.String()},
		{"{{ThrottleInterval}}", strconv.Itoa(resolved.ThrottleInterval)},
		{"{{WorkingDirectory}}", escapeValue(resolved.WorkingDirectory2)},
		{"{{StdOutPath}}", escapeValue(resolved.StdOutPath)},
		{"{{StdErrPath}}", escapeValue(resolved.StdErrPath)},
		{"{{EnvironmentBlock}}", environment},
	} {
		rendered = strings.ReplaceAll(rendered, replacement.key, replacement.value)
	}
	if strings.Contains(rendered, "{{") {
		return "", errors.New("the LaunchAgent template was rendered with an unfilled placeholder")
	}
	// Confirm the document is well formed and still says what it should before
	// anything writes it.
	document, err := parsePlist([]byte(rendered))
	if err != nil {
		return "", fmt.Errorf("rendered LaunchAgent is not a valid plist: %w", err)
	}
	if got, ok := stringValue(document, "Label"); !ok || got != resolved.Label {
		return "", fmt.Errorf("rendered LaunchAgent label is %q, want %q", got, resolved.Label)
	}
	gotArguments, ok := stringSliceValue(document, "ProgramArguments")
	if !ok || len(gotArguments) != len(arguments) {
		return "", errors.New("rendered LaunchAgent lost a program argument")
	}
	for index, want := range arguments {
		if gotArguments[index] != want {
			return "", fmt.Errorf("rendered LaunchAgent argument %d is %q, want %q", index+1, gotArguments[index], want)
		}
	}
	if stdout, ok := stringValue(document, "StandardOutPath"); !ok || stdout != resolved.StdOutPath {
		return "", errors.New("rendered LaunchAgent stdout path does not match the requested log directory")
	}
	if len(resolved.Env) > 0 {
		environment, ok := document["EnvironmentVariables"].(map[string]any)
		if !ok || len(environment) != len(resolved.Env) {
			return "", errors.New("rendered LaunchAgent lost an environment variable")
		}
		for name, want := range resolved.Env {
			if got, ok := environment[name].(string); !ok || got != want {
				return "", fmt.Errorf("rendered LaunchAgent environment variable %q did not survive the round trip", name)
			}
		}
	}
	return rendered, nil
}

func escapeValue(value string) string {
	if err := safeValue("path", value); err != nil {
		// RenderLaunchAgent checks paths before rendering; this keeps the helper
		// total so a caller cannot panic the render.
		return ""
	}
	return escape(value)
}

// safeValue rejects characters that cannot legally appear in XML text, which also
// blocks an attempt to break out of the surrounding element.
func safeValue(what, value string) error {
	if value == "" {
		return fmt.Errorf("%s is empty", what)
	}
	for _, character := range value {
		if character == 0 || (character < 0x20 && character != '\t' && character != '\n' && character != '\r') {
			return fmt.Errorf("%s contains control character %#x", what, character)
		}
	}
	return nil
}

func escape(value string) string {
	buffer := &bytes.Buffer{}
	_ = xml.EscapeText(buffer, []byte(value))
	return buffer.String()
}

func validEnvName(name string) error {
	if name == "" {
		return errors.New("environment variable name is empty")
	}
	if strings.ContainsAny(name, "=\r\n \t") {
		return fmt.Errorf("environment variable name %q is invalid", name)
	}
	for index, character := range name {
		switch {
		case character >= 'A' && character <= 'Z', character >= 'a' && character <= 'z', character == '_':
		case character >= '0' && character <= '9' && index > 0:
		default:
			return fmt.Errorf("environment variable name %q is invalid", name)
		}
	}
	return nil
}

// Owned reports whether existing plist content was produced by this installer for
// this binary. An unrelated file at the same path is never overwritten silently.
func Owned(content []byte, label, binaryPath string) bool {
	document, err := parsePlist(content)
	if err != nil {
		return false
	}
	documentedLabel, ok := stringValue(document, "Label")
	if !ok || strings.TrimSpace(documentedLabel) != label {
		return false
	}
	arguments, ok := stringSliceValue(document, "ProgramArguments")
	if !ok || len(arguments) < 2 {
		return false
	}
	if filepath.Clean(arguments[0]) != filepath.Clean(binaryPath) {
		return false
	}
	return arguments[1] == "serve"
}
