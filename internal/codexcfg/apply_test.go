package codexcfg

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

func TestSetRootStringReplacesInPlace(t *testing.T) {
	original := `# keep me
model = "gpt-5.6-sol"
model_provider = "custom"   # trailing note
notify = ["a", "b"]

[desktop]
enable = true # inline
other = "x"
`
	updated, err := setRootString(original, "openai_base_url", "http://127.0.0.1:4317/v1")
	if err != nil {
		t.Fatalf("set: %v", err)
	}
	if !strings.Contains(updated, `openai_base_url = "http://127.0.0.1:4317/v1"`) {
		t.Fatalf("key not added:\n%s", updated)
	}
	// The new key belongs to the root section, before [desktop].
	if strings.Index(updated, "openai_base_url") > strings.Index(updated, "[desktop]") {
		t.Fatalf("key was inserted after the table header:\n%s", updated)
	}
	if !strings.Contains(updated, "notify = [\"a\", \"b\"]") {
		t.Fatalf("array value was damaged:\n%s", updated)
	}
	if !strings.Contains(updated, "enable = true # inline") {
		t.Fatalf("table body was damaged:\n%s", updated)
	}
	if !strings.Contains(updated, "# keep me") {
		t.Fatalf("comment was lost:\n%s", updated)
	}

	// Replacing the same key twice must not duplicate it.
	again, err := setRootString(updated, "openai_base_url", "http://127.0.0.1:4400/v1")
	if err != nil {
		t.Fatalf("replace: %v", err)
	}
	if strings.Count(again, "openai_base_url") != 1 {
		t.Fatalf("key was duplicated:\n%s", again)
	}
	if !strings.Contains(again, `= "http://127.0.0.1:4400/v1"`) {
		t.Fatalf("value was not replaced:\n%s", again)
	}
}

func TestSetRootStringKeepsTrailingComment(t *testing.T) {
	updated, err := setRootString("model_provider = \"custom\" # keep this note\n", "model_provider", "router")
	if err != nil {
		t.Fatalf("set: %v", err)
	}
	if !strings.Contains(updated, `model_provider = "router" # keep this note`) {
		t.Fatalf("comment handling wrong:\n%s", updated)
	}
}

func TestRemoveRootKeyKeepsTrailingCommentOnOtherKeys(t *testing.T) {
	original := `model = "x"
model_provider = "custom"  # remove me
[profiles.dev]
model = "y"
`
	updated, err := removeRootKey(original, "model_provider")
	if err != nil {
		t.Fatalf("remove: %v", err)
	}
	if strings.Contains(updated, "model_provider") {
		t.Fatalf("key still present:\n%s", updated)
	}
	if !strings.Contains(updated, "[profiles.dev]\nmodel = \"y\"") {
		t.Fatalf("table damaged:\n%s", updated)
	}
}

func TestSetRootStringRefusesMultiLineValues(t *testing.T) {
	_, err := setRootString("openai_base_url = [\n  \"a\",\n]\n", "openai_base_url", "http://127.0.0.1:4317/v1")
	if err == nil || !strings.Contains(err.Error(), "by hand") {
		t.Fatalf("expected a refusal for a multi-line value, got %v", err)
	}
}

func TestRootStringIgnoresTableKeys(t *testing.T) {
	text := "[model_providers.custom]\nopenai_base_url = \"https://elsewhere.example/v1\"\n"
	value, found, err := RootString(text, "openai_base_url")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if found {
		t.Fatalf("a key inside a table was treated as root: %q", value)
	}
}

func TestBuildPlanPreviewDoesNotWrite(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "config.toml")
	original := "# user comment\nmodel = \"gpt-5.6-sol\"\nmodel_provider = \"legacy\"\n\n[features]\nshell_tool = true\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	desired := Desired{
		OpenAIBaseURL:    "http://127.0.0.1:4317/v1",
		ModelCatalogJSON: filepath.Join(directory, "catalog.json"),
	}
	plan, err := BuildPlan(path, desired)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if plan.AlreadyCurrent {
		t.Fatal("plan reported no changes")
	}
	preview, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(preview) != original {
		t.Fatal("BuildPlan modified the file")
	}
	if _, err := os.Stat(BackupPath(path)); !os.IsNotExist(err) {
		t.Fatal("BuildPlan wrote a backup")
	}

	described := plan.Describe()
	for _, want := range []string{"+ openai_base_url", "~ model_provider", "native account authentication"} {
		if !strings.Contains(described, strings.TrimPrefix(want, "~ ")) {
			t.Fatalf("plan description omitted %q:\n%s", want, described)
		}
	}

	if _, err := Apply(plan, false); err == nil {
		t.Fatal("Apply accepted an unconfirmed plan")
	}
	backup, err := Apply(plan, true)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if backup == "" {
		t.Fatal("no backup path reported")
	}
	final, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(final)
	for _, want := range []string{
		"# user comment",
		"model = \"gpt-5.6-sol\"",
		"shell_tool = true",
		`openai_base_url = "http://127.0.0.1:4317/v1"`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("updated config lost %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "model_provider") {
		t.Fatalf("model_provider survived:\n%s", text)
	}
	restored, err := os.ReadFile(backup)
	if err != nil {
		t.Fatal(err)
	}
	if string(restored) != original {
		t.Fatal("backup does not match the original file")
	}

	if err := Restore(path, backup); err != nil {
		t.Fatalf("restore: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != original {
		t.Fatalf("restore did not reproduce the original:\n%s", after)
	}
}

func TestBuildPlanIsIdempotent(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "config.toml")
	desired := Desired{OpenAIBaseURL: "http://127.0.0.1:4317/v1", ModelCatalogJSON: filepath.Join(directory, "catalog.json")}
	if err := os.WriteFile(path, []byte("model = \"x\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	plan, err := BuildPlan(path, desired)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(plan, true); err != nil {
		t.Fatal(err)
	}
	second, err := BuildPlan(path, desired)
	if err != nil {
		t.Fatal(err)
	}
	if !second.Empty() {
		t.Fatalf("second plan still wants changes: %s", second.Describe())
	}
	if _, err := Apply(second, true); err != nil {
		t.Fatal(err)
	}
}

func TestBuildPlanRefusesBrokenTOML(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "config.toml")
	if err := os.WriteFile(path, []byte("model = \n[broken\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := BuildPlan(path, Desired{OpenAIBaseURL: "http://127.0.0.1:4317/v1", ModelCatalogJSON: filepath.Join(directory, "c.json")}); err == nil {
		t.Fatal("expected a refusal for invalid TOML")
	}
}

func TestBuildPlanCreatesMissingFile(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "nested", "config.toml")
	plan, err := BuildPlan(path, Desired{OpenAIBaseURL: "http://127.0.0.1:4317/v1", ModelCatalogJSON: filepath.Join(directory, "c.json")})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Existed {
		t.Fatal("plan claims the file existed")
	}
	if _, err := Apply(plan, true); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "openai_base_url") {
		t.Fatalf("created file is wrong:\n%s", data)
	}
}

func TestDesiredValidationRejectsNonLoopbackURL(t *testing.T) {
	// The contract is that openai_base_url points at this router, so anything that
	// would send every model request somewhere else is refused, including a plain
	// http URL that merely looks well formed.
	rejected := map[string]string{
		"https":               "https://router.example/v1",
		"external http":       "http://198.51.100.7:4317/v1",
		"external hostname":   "http://router.example:4317/v1",
		"public ipv6":         "http://[2001:db8::1]:4317/v1",
		"port out of range":   "http://127.0.0.1:70000/v1",
		"port zero":           "http://127.0.0.1:0/v1",
		"no port":             "http://127.0.0.1/v1",
		"not a number":        "http://127.0.0.1:http/v1",
		"no scheme":           "127.0.0.1:4317/v1",
		"embedded credential": "http://u:p@127.0.0.1:4317/v1",
		"query string":        "http://127.0.0.1:4317/v1?x=1",
		"empty":               "",
	}
	for name, value := range rejected {
		err := Desired{OpenAIBaseURL: value, ModelCatalogJSON: "/tmp/c.json"}.Validate()
		if err == nil {
			t.Fatalf("the %s URL %q was accepted", name, value)
		}
	}
	for _, value := range []string{
		"http://127.0.0.1:4317/v1",
		"http://localhost:4317/v1",
		"http://[::1]:4317/v1",
		"http://127.8.9.10:1/",
		"http://LOCALHOST:65535",
	} {
		if err := (Desired{OpenAIBaseURL: value, ModelCatalogJSON: "/tmp/c.json"}).Validate(); err != nil {
			t.Fatalf("the loopback URL %q should be accepted: %v", value, err)
		}
	}
	if err := (Desired{OpenAIBaseURL: "http://127.0.0.1:4317/v1", ModelCatalogJSON: "catalog.json"}).Validate(); err == nil {
		t.Fatal("expected a relative catalog path to be rejected")
	}
}

func TestApplyRefusesAStalePlan(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "config.toml")
	const original = `model = "gpt-5.6-sol"
`
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	desired := Desired{OpenAIBaseURL: "http://127.0.0.1:4317/v1", ModelCatalogJSON: filepath.Join(directory, "catalog.json")}
	plan, err := BuildPlan(path, desired)
	if err != nil {
		t.Fatal(err)
	}

	// The user edits the file after the plan was reviewed. Applying the stale plan
	// would silently drop that edit, so the write is refused and the edit survives.
	const userEdit = `model = "gpt-5.6-sol"
model_reasoning_effort = "high"
`
	if err := os.WriteFile(path, []byte(userEdit), 0o600); err != nil {
		t.Fatal(err)
	}
	backup, err := Apply(plan, true)
	if err == nil {
		t.Fatal("a plan built from stale content was applied")
	}
	if !strings.Contains(err.Error(), "changed after this plan was built") {
		t.Fatalf("error = %v", err)
	}
	if backup != "" {
		t.Fatalf("no backup should have been written, got %q", backup)
	}
	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(onDisk) != userEdit {
		t.Fatalf("the user's edit was not preserved:\n%s", onDisk)
	}
	if _, err := os.Stat(BackupPath(path)); !os.IsNotExist(err) {
		t.Fatal("a stale plan left a backup behind")
	}

	// A fresh plan against the current content applies cleanly and keeps the edit.
	fresh, err := BuildPlan(path, desired)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(fresh, true); err != nil {
		t.Fatalf("the rebuilt plan failed to apply: %v", err)
	}
	onDisk, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(onDisk), `model_reasoning_effort = "high"`) || !strings.Contains(string(onDisk), "openai_base_url") {
		t.Fatalf("the applied file lost content:\n%s", onDisk)
	}

	// A file that appears after planning an absent one is also refused.
	absent := filepath.Join(directory, "new", "config.toml")
	absentPlan, err := BuildPlan(absent, desired)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(absent), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(absent, []byte(`model = "surprise"`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(absentPlan, true); err == nil {
		t.Fatal("a file that appeared after planning should stop the write")
	}
}

func TestBuildPlanHandlesSparseAndTableOnlyFiles(t *testing.T) {
	desired := Desired{OpenAIBaseURL: "http://127.0.0.1:4317/v1", ModelCatalogJSON: "/tmp/catalog.json"}
	cases := map[string]string{
		"table only":                         "[projects.\"/example\"]\ntrust_level = \"trusted\"\n",
		"table only without a final newline": "[projects.\"/example\"]\ntrust_level = \"trusted\"",
		"empty file":                         "",
		"only a comment":                     "# nothing here\n",
		"only blank lines":                   "\n\n",
		"root key then table":                "model = \"gpt-5\"\n\n[profiles.dev]\nmodel = \"gpt-5\"\n",
		"crlf table only":                    "[a]\r\nb = 1\r\n",
	}
	for name, text := range cases {
		directory := t.TempDir()
		path := filepath.Join(directory, "config.toml")
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		plan, err := BuildPlan(path, desired)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		// The keys must land in the root section, ahead of any table header.
		index := strings.Index(plan.UpdatedText, "openai_base_url")
		table := strings.Index(plan.UpdatedText, "[")
		if index < 0 {
			t.Fatalf("%s: the key was not added:\n%q", name, plan.UpdatedText)
		}
		if table >= 0 && index > table {
			t.Fatalf("%s: the key was added inside a table:\n%q", name, plan.UpdatedText)
		}
		if _, err := toml.Decode(plan.UpdatedText, &map[string]any{}); err != nil {
			t.Fatalf("%s: the updated file does not parse: %v\n%q", name, err, plan.UpdatedText)
		}
		var decoded map[string]any
		if _, err := toml.Decode(plan.UpdatedText, &decoded); err != nil {
			t.Fatal(err)
		}
		if decoded["openai_base_url"] != desired.OpenAIBaseURL {
			t.Fatalf("%s: openai_base_url = %v", name, decoded["openai_base_url"])
		}
		// Unrelated content survives untouched.
		if !strings.Contains(plan.UpdatedText, text[:min(len(text), 5)]) && strings.TrimSpace(text) != "" {
			t.Fatalf("%s: the original content disappeared:\n%q", name, plan.UpdatedText)
		}
	}
}

func TestBuildPlanRejectsUnsupportedFilesWithoutPanic(t *testing.T) {
	desired := Desired{OpenAIBaseURL: "http://127.0.0.1:4317/v1", ModelCatalogJSON: "/tmp/catalog.json"}
	cases := map[string]string{
		"unbalanced quotes": "model = \"unterminated\n",
		"unclosed table":    "[profiles.dev\nmodel = \"gpt-5\"\n",
		"duplicate key":     "model = \"a\"\nmodel = \"b\"\n",
	}
	for name, text := range cases {
		directory := t.TempDir()
		path := filepath.Join(directory, "config.toml")
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		func() {
			defer func() {
				if recovered := recover(); recovered != nil {
					t.Fatalf("%s: BuildPlan panicked: %v", name, recovered)
				}
			}()
			if _, err := BuildPlan(path, desired); err == nil {
				t.Fatalf("%s: an unparseable file should be refused, not rewritten", name)
			}
		}()
	}
}

func min(left, right int) int {
	if left < right {
		return left
	}
	return right
}

func TestSnippetIsDocumentedConfiguration(t *testing.T) {
	text := Snippet(Desired{OpenAIBaseURL: "http://127.0.0.1:4317/v1", ModelCatalogJSON: "/home/user/.codex/catalog.json"})
	for _, want := range []string{"openai_base_url", "model_catalog_json", "model_provider"} {
		if !strings.Contains(text, want) {
			t.Fatalf("snippet omitted %s:\n%s", want, text)
		}
	}
	if strings.Contains(text, "model_provider =") {
		t.Fatalf("snippet must not set model_provider:\n%s", text)
	}
}

func TestSplitLinesKeepsCarriageReturns(t *testing.T) {
	updated, err := setRootString("model = \"x\"\r\nnotify = 1\r\n", "openai_base_url", "http://127.0.0.1:4317/v1")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(updated, "\r\n") < 2 {
		t.Fatalf("CRLF endings were not preserved:\n%q", updated)
	}
}
