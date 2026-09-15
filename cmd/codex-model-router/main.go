// Command codex-model-router runs a loopback proxy that lets one Codex session use
// OpenAI models and self-hosted models side by side.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/zydtiger/codex-model-router/internal/catalog"
	"github.com/zydtiger/codex-model-router/internal/config"
	"github.com/zydtiger/codex-model-router/internal/routing"
	"github.com/zydtiger/codex-model-router/internal/serve"
	"github.com/zydtiger/codex-model-router/internal/service"
)

// version is reported by the version subcommand and by /healthz.
const version = routing.Version

// Exit codes let a supervisor tell a broken configuration apart from a runtime
// failure without parsing messages.
const (
	exitOK     = 0
	exitError  = 1
	exitUsage  = 2
	exitConfig = 3
)

// errUsage marks a command-line mistake, which maps to exitUsage.
var errUsage = errors.New("usage")

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return exitUsage
	}
	command, rest := args[0], args[1:]

	var err error
	switch command {
	case "serve":
		err = cmdServe(rest, stdout, stderr)
	case "validate":
		err = cmdValidate(rest, stdout)
	case "catalog":
		err = cmdCatalog(rest, stdout)
	case "service":
		err = cmdService(rest, stdout)
	case "healthcheck":
		err = cmdHealthCheck(rest, stdout)
	case "version":
		fmt.Fprintf(stdout, "codex-model-router %s\n", version)
		return exitOK
	case "help", "-h", "--help":
		usage(stdout)
		return exitOK
	default:
		fmt.Fprintf(stderr, "unknown command %q\n\n", command)
		usage(stderr)
		return exitUsage
	}

	switch {
	case err == nil:
		return exitOK
	case errors.Is(err, flag.ErrHelp):
		// flag already printed the command's own usage.
		return exitOK
	case errors.Is(err, errUsage):
		fmt.Fprintf(stderr, "error: %v\n", err)
		return exitUsage
	case errors.Is(err, config.ErrInvalid):
		fmt.Fprintf(stderr, "%v\n", err)
		return exitConfig
	default:
		fmt.Fprintf(stderr, "error: %v\n", err)
		return exitError
	}
}

func usage(w io.Writer) {
	defaultConfig, _ := defaultConfigPath()
	fmt.Fprintf(w, `codex-model-router %s - route Codex models between native ChatGPT and self-hosted servers

Usage:
  codex-model-router <command> [flags]

Commands:
  serve                     Run the router in the foreground
  validate                  Check a configuration without opening a port
  catalog generate          Merge native and self-hosted models into a catalog JSON
  catalog print-example     Print a starting-point configuration
  service preview           Print the platform service definition
  service install           Install and start a user service (macOS/Linux, no sudo)
  service status            Report the service state and router health
  service uninstall         Stop the service and remove its definition
  healthcheck               Probe a running router; exit 0 when healthy
  version                   Print the version

A configuration path comes from --config, then $CODEX_MODEL_ROUTER_CONFIG, then %s.
Run "codex-model-router <command> -h" for a command's own flags.
`, version, defaultConfig)
}

// defaultConfigPath is where the configuration is looked for when nothing else
// names it.
func defaultConfigPath() (string, error) {
	if fromEnv := os.Getenv("CODEX_MODEL_ROUTER_CONFIG"); fromEnv != "" {
		return filepath.Abs(fromEnv)
	}
	if base := os.Getenv("XDG_CONFIG_HOME"); base != "" {
		return filepath.Join(base, "codex-model-router", "config.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("no home directory; pass --config: %w", err)
	}
	return filepath.Join(home, ".config", "codex-model-router", "config.json"), nil
}

// loadConfig resolves the --config value and loads the file.
func loadConfig(explicit string) (*config.Config, string, error) {
	path := explicit
	if path == "" {
		resolved, err := defaultConfigPath()
		if err != nil {
			return nil, "", err
		}
		path = resolved
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, "", err
	}
	if _, statErr := os.Stat(absolute); statErr != nil && explicit == "" {
		example, _ := defaultConfigPath()
		return nil, "", fmt.Errorf("%w: no configuration at %s. Create one with: codex-model-router catalog print-example --out %s",
			config.ErrInvalid, absolute, example)
	}
	cfg, err := config.Load(absolute)
	if err != nil {
		return nil, absolute, err
	}
	return cfg, absolute, nil
}

// newLogger builds the router logger. The format is fixed line-oriented text
// because the output normally lands in a launchd log file.
func newLogger(cfg *config.Config, override string, w io.Writer) *slog.Logger {
	level := override
	if level == "" && cfg != nil {
		level = cfg.Log.Level
	}
	var slogLevel slog.Level
	switch level {
	case "debug":
		slogLevel = slog.LevelDebug
	case "error":
		slogLevel = slog.LevelError
	case "info", "":
		slogLevel = slog.LevelInfo
	default:
		slogLevel = slog.LevelWarn
	}
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slogLevel, ReplaceAttr: redactAttribute}))
}

// redactedAttributeNames are never printed with their values.
var redactedAttributeNames = []string{"key", "token", "secret", "authorization", "cookie", "header", "password", "credential"}

// redactAttribute removes the value of any attribute whose name suggests a secret.
// The router never logs bodies, so this is a second layer, not the only one.
func redactAttribute(_ []string, attribute slog.Attr) slog.Attr {
	name := strings.ToLower(attribute.Key)
	for _, word := range redactedAttributeNames {
		if strings.Contains(name, word) {
			return slog.String(attribute.Key, "[redacted]")
		}
	}
	return attribute
}

// parseFlags parses a command's arguments. Anything the flag package rejects is a
// usage error, so it maps to the usage exit code rather than a generic failure.
func parseFlags(flags *flag.FlagSet, args []string) error {
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return err
		}
		return fmt.Errorf("%w: %v", errUsage, err)
	}
	return nil
}

// registerConfigFlag adds the flags shared by the commands that read a file.
func registerConfigFlag(flags *flag.FlagSet) *string {
	var path string
	flags.StringVar(&path, "config", "", "router configuration JSON (default: $CODEX_MODEL_ROUTER_CONFIG or ~/.config/codex-model-router/config.json)")
	return &path
}

// cmdServe runs the router until the process is asked to stop.
func cmdServe(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := registerConfigFlag(flags)
	logLevel := flags.String("log-level", "", "override log.level: debug, info, warn, or error")
	quiet := flags.Bool("quiet", false, "do not print the start-up summary")
	shutdownTimeout := flags.Duration("shutdown-timeout", 15*time.Second, "grace given to in-flight requests on exit; 0 closes immediately")
	if err := parseFlags(flags, args); err != nil {
		return err
	}
	if flags.NArg() > 0 {
		return fmt.Errorf("%w: serve takes no positional arguments", errUsage)
	}
	if *shutdownTimeout < 0 {
		return fmt.Errorf("%w: --shutdown-timeout must not be negative", errUsage)
	}

	cfg, _, err := loadConfig(*configPath)
	if err != nil {
		return err
	}
	logger := newLogger(cfg, *logLevel, stderr)
	chatGPTProxy, err := routing.LoadChatGPTProxy(os.LookupEnv)
	if err != nil {
		return err
	}
	router, err := routing.New(cfg, routing.Options{Logger: logger, Env: os.LookupEnv, ChatGPTProxy: chatGPTProxy})
	if err != nil {
		return err
	}
	server, err := serve.New(router, serve.Options{Host: cfg.Listen.Host, Port: cfg.Listen.Port})
	if err != nil {
		return err
	}
	router.SetBoundAddr(server.Addr().String())

	if !*quiet {
		fmt.Fprintf(stdout, "codex-model-router %s listening on http://%s%s\n", version, server.Addr().String(), cfg.BasePath)
		describeRoutes(stdout, cfg, server.Addr().String())
	}
	logger.Info("router started",
		"address", server.Addr().String(),
		"base_path", cfg.BasePath,
		"routed_models", len(cfg.RoutedModelIDs()),
		"native_models", len(cfg.NativeModelIDs()),
	)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	served := make(chan error, 1)
	go func() { served <- server.Serve() }()
	select {
	case err := <-served:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("the router stopped: %w", err)
		}
		return nil
	case <-ctx.Done():
	}

	stop()
	logger.Info("stopping", "grace", shutdownTimeout.String())
	shutdownCtx, cancel := context.WithTimeout(context.Background(), *shutdownTimeout)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		return err
	}
	logger.Info("router stopped")
	if !*quiet {
		fmt.Fprintf(stdout, "stopped: %s\n", jsonCompact(router.Health()))
	}
	return nil
}

// describeRoutes prints the routing table so the operator can see, at start-up,
// exactly which model ID goes where. Each line names the URL an upstream request
// will actually be sent to.
func describeRoutes(w io.Writer, cfg *config.Config, address string) {
	for _, route := range cfg.Routes {
		fmt.Fprintf(w, "  route %-14s POST %s/responses\n", route.Name, route.BaseURL)
		fmt.Fprintf(w, "    %-14s %s\n", "models:", strings.Join(route.Models, ", "))
		fmt.Fprintf(w, "    %-14s %s\n", "reasoning:", describeReasoning(route))
		fmt.Fprintf(w, "    %-14s %s\n", "tools:", describeTools(route))
		fmt.Fprintf(w, "    %-14s %s\n", "credential:", describeAuth(route))
	}
	fmt.Fprintf(w, "  native           %s\n", cfg.Native.ChatGPTBaseURL)
	fmt.Fprintf(w, "    %-14s %s\n", "models:", strings.Join(cfg.NativeModelIDs(), ", "))
	fmt.Fprintf(w, "    %-14s %s\n", "fallback:", cfg.Native.APIBaseURL)
	fmt.Fprintf(w, "  router           POST http://%s%s/responses\n", address, cfg.BasePath)
	fmt.Fprintf(w, "  health           GET  http://%s%s\n", address, routing.HealthPath)
}

// describeAuth reports where a route's credential comes from without revealing it.
func describeAuth(route config.Route) string {
	if route.Auth == nil || route.Auth.APIKeyEnv == "" {
		return "none configured"
	}
	return "environment variable " + route.Auth.APIKeyEnv
}

func describeReasoning(route config.Route) string {
	switch route.Reasoning.Adapter {
	case config.AdapterNone, "":
		return "none (the reasoning fields are forwarded unchanged)"
	case config.AdapterReasoningToChatTemplate:
		if len(route.Reasoning.ChatTemplateKwargs) == 0 {
			return "reasoning_to_chat_template with no chat_template_kwargs (nothing is injected)"
		}
		keys := make([]string, 0, len(route.Reasoning.ChatTemplateKwargs))
		for key := range route.Reasoning.ChatTemplateKwargs {
			keys = append(keys, key)
		}
		sortStrings(keys)
		return fmt.Sprintf("reasoning_to_chat_template: chat_template_kwargs{%s}", strings.Join(keys, ", "))
	default:
		return route.Reasoning.Adapter
	}
}

// describeTools reports the route's tool translation settings independently of
// the reasoning adapter.
func describeTools(route config.Route) string {
	adapter := route.Tools.NamespaceAdapter
	custom := route.Tools.CustomAdapter
	if (adapter == "" || adapter == config.NamespaceAdapterNone) && (custom == "" || custom == config.CustomAdapterNone) {
		return "none (tool declarations are forwarded unchanged)"
	}
	parts := make([]string, 0, 2)
	if adapter != "" && adapter != config.NamespaceAdapterNone {
		loading := route.Tools.SchemaLoading
		if loading == "" {
			loading = config.SchemaLoadingAll
		}
		if loading == config.SchemaLoadingAll {
			parts = append(parts, adapter+", schema_loading=all (full flattened schema inventory is sent)")
		} else {
			parts = append(parts, adapter+", schema_loading="+loading+" (a loader tool discloses schemas on demand)")
		}
	}
	if custom != "" && custom != config.CustomAdapterNone {
		parts = append(parts, custom+" (custom tools are bridged to function calls)")
	}
	return strings.Join(parts, "; ")
}

// cmdValidate reports what a configuration means without opening a port.
func cmdValidate(args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("validate", flag.ContinueOnError)
	configPath := registerConfigFlag(flags)
	asJSON := flags.Bool("json", false, "print a machine-readable summary")
	if err := parseFlags(flags, args); err != nil {
		return err
	}
	if flags.NArg() > 0 {
		return fmt.Errorf("%w: validate takes no positional arguments", errUsage)
	}
	cfg, path, err := loadConfig(*configPath)
	if err != nil {
		return err
	}
	catalogState := "not configured"
	if cfg.Catalog.OutputFile != "" {
		entries, countErr := countCatalogEntries(cfg.Catalog.OutputFile)
		switch {
		case countErr != nil && os.IsNotExist(unwrap(countErr)):
			catalogState = "missing: " + cfg.Catalog.OutputFile + " (run 'catalog generate')"
		case countErr != nil:
			catalogState = "unreadable: " + countErr.Error()
		default:
			catalogState = fmt.Sprintf("%s (%d entries)", cfg.Catalog.OutputFile, entries)
		}
	}

	if *asJSON {
		routes := make([]map[string]any, 0, len(cfg.Routes))
		for _, route := range cfg.Routes {
			routes = append(routes, map[string]any{
				"name":      route.Name,
				"base_url":  route.BaseURL,
				"models":    route.Models,
				"reasoning": route.Reasoning.Adapter,
				"tools": map[string]any{
					"namespace_adapter": route.Tools.NamespaceAdapter,
					"custom_adapter":    route.Tools.CustomAdapter,
					"schema_loading":    route.Tools.SchemaLoading,
				},
				"auth": describeAuth(route),
			})
		}
		return writeJSON(stdout, map[string]any{
			"ok":                   true,
			"config":               path,
			"listen":               cfg.Addr(),
			"base_path":            cfg.BasePath,
			"health_path":          routing.HealthPath,
			"max_request_bytes":    cfg.MaxRequestBytes,
			"routes":               routes,
			"native_models":        cfg.NativeModelIDs(),
			"preserve_client_auth": cfg.PreserveClientAuth(),
			"catalog":              catalogState,
		})
	}
	fmt.Fprintf(stdout, "configuration is valid: %s\n", path)
	describeRoutes(stdout, cfg, cfg.Addr())
	fmt.Fprintf(stdout, "  catalog          %s\n", catalogState)
	if cfg.Listen.Port == 0 {
		fmt.Fprintln(stdout, "  note             listen.port is 0, so a random port is chosen at start-up; use a fixed port when configuring Codex")
	}
	return nil
}

func countCatalogEntries(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return catalog.CountModels(data)
}

// unwrap resolves a wrapped error for the not-exist check.
func unwrap(err error) error {
	for current := err; current != nil; current = errors.Unwrap(current) {
		if errors.Is(current, os.ErrNotExist) {
			return current
		}
	}
	return err
}

// cmdCatalog generates the combined model catalog.
func cmdCatalog(args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("%w: catalog needs generate or print-example", errUsage)
	}
	subcommand, rest := args[0], args[1:]
	flags := flag.NewFlagSet("catalog "+subcommand, flag.ContinueOnError)
	configPath := registerConfigFlag(flags)
	out := flags.String("out", "", "write to this path instead of stdout")
	if err := parseFlags(flags, rest); err != nil {
		return err
	}
	if flags.NArg() > 0 {
		return fmt.Errorf("%w: catalog %s takes no positional arguments", errUsage, subcommand)
	}

	switch subcommand {
	case "print-example":
		example, err := config.Example()
		if err != nil {
			return err
		}
		return emitBytes(stdout, example, *out)
	case "generate":
		cfg, _, err := loadConfig(*configPath)
		if err != nil {
			return err
		}
		if len(cfg.Catalog.Models) == 0 {
			fmt.Fprintln(stdout, "warning: catalog.models is empty, so the output repeats the native catalog only")
		}
		data, destination, err := catalog.Generate(cfg, *out)
		if err != nil {
			return err
		}
		entries, err := catalog.CountModels(data)
		if err != nil {
			return err
		}
		if destination == "" {
			return emitBytes(stdout, data, "")
		}
		fmt.Fprintf(stdout, "wrote %s (%d model entries)\n", destination, entries)
		fmt.Fprintf(stdout, "Set model_catalog_json in your Codex configuration to %q; see docs/codex-desktop.md.\n", destination)
		return nil
	default:
		return fmt.Errorf("%w: unknown catalog subcommand %q", errUsage, subcommand)
	}
}

func emitBytes(stdout io.Writer, data []byte, out string) error {
	if out == "" {
		_, err := stdout.Write(data)
		return err
	}
	if err := os.WriteFile(out, data, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", out, err)
	}
	fmt.Fprintf(stdout, "wrote %s\n", out)
	return nil
}

// cmdService previews and manages the launchd LaunchAgent.
func cmdService(args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("%w: service needs preview, install, status, or uninstall", errUsage)
	}
	subcommand, rest := args[0], args[1:]
	// The subcommand is checked before the configuration is loaded, so a typo is a
	// usage error rather than a configuration failure.
	switch subcommand {
	case "preview", "install", "status", "uninstall":
	default:
		return fmt.Errorf("%w: unknown service subcommand %q; use preview, install, status, or uninstall", errUsage, subcommand)
	}
	flags := flag.NewFlagSet("service "+subcommand, flag.ContinueOnError)
	configPath := registerConfigFlag(flags)
	var options service.Options
	options.Platform = runtime.GOOS
	flags.StringVar(&options.Label, "label", service.DefaultLabel, "service label")
	flags.StringVar(&options.BinaryPath, "bin", "", "router binary the agent runs (default: $CODEX_MODEL_ROUTER_BIN or ~/.local/lib/codex-model-router/codex-model-router)")
	flags.StringVar(&options.UnitDir, "unit-dir", "", "systemd user unit directory (default: $XDG_CONFIG_HOME/systemd/user or ~/.config/systemd/user)")
	flags.StringVar(&options.PlistDir, "plist-dir", "", "LaunchAgents directory (default: ~/Library/LaunchAgents)")
	flags.StringVar(&options.LogDir, "log-dir", "", "directory for the router's log files (default: ~/Library/Logs/<label>)")
	flags.StringVar(&options.WorkingDirectory, "working-directory", "", "working directory for the agent process")
	throttle := flags.Int("throttle-interval", 10, "minimum seconds between service restarts")
	shutdownTimeout := flags.String("shutdown-timeout", "", "pass a grace window to serve, for example 15s")
	var environment stringList
	flags.Var(&environment, "env", "extra environment for the agent as KEY=VALUE; repeat for more")
	force := flags.Bool("force", false, "replace an existing service definition")
	dryRun := flags.Bool("dry-run", false, "report what would happen without writing or changing services")
	if err := parseFlags(flags, rest); err != nil {
		return err
	}
	if flags.NArg() > 0 {
		return fmt.Errorf("%w: service %s takes no positional arguments", errUsage, subcommand)
	}
	options.ThrottleInterval = *throttle

	cfg, resolvedConfigPath, err := loadConfig(*configPath)
	if err != nil {
		return err
	}
	options.ConfigPath = resolvedConfigPath
	if options.BinaryPath == "" {
		if fromEnv := os.Getenv("CODEX_MODEL_ROUTER_BIN"); fromEnv != "" {
			options.BinaryPath = fromEnv
		} else {
			defaultBinary, err := service.DefaultBinaryPath()
			if err != nil {
				return err
			}
			options.BinaryPath = defaultBinary
		}
	}
	if options.Platform == "darwin" && options.UnitDir != "" {
		return fmt.Errorf("%w: --unit-dir applies only to Linux", errUsage)
	}
	if options.Platform == "darwin" && options.PlistDir == "" {
		options.PlistDir, err = service.DefaultPlistDir()
		if err != nil {
			return err
		}
	}
	if options.Platform == "darwin" && options.LogDir == "" {
		options.LogDir, err = service.DefaultLogDir(options.Label)
		if err != nil {
			return err
		}
	}
	if *shutdownTimeout != "" {
		if _, err := time.ParseDuration(*shutdownTimeout); err != nil {
			return fmt.Errorf("%w: --shutdown-timeout must be a duration: %v", errUsage, err)
		}
		options.ExtraArgs = []string{"--shutdown-timeout", *shutdownTimeout}
	}
	env, err := parseEnvironment(environment)
	if err != nil {
		return fmt.Errorf("%w: %v", errUsage, err)
	}
	options.Env = env

	host := cfg.Listen.Host
	if host == "localhost" {
		host = "127.0.0.1"
	}
	manager := &service.Manager{
		Options: options,
		Runner:  service.ExecRunner{},
		HealthProbe: func(ctx context.Context) (map[string]any, error) {
			return serve.Health(ctx, host, cfg.Listen.Port, routing.HealthPath)
		},
	}

	switch subcommand {
	case "preview":
		rendered, resolved, err := manager.Preview()
		if err != nil {
			return err
		}
		fmt.Fprint(stdout, rendered)
		if resolved.UnitPath != "" {
			fmt.Fprintf(stdout, "# unit path: %s\n# logs: journalctl --user -u %s\n", resolved.UnitPath, filepath.Base(resolved.UnitPath))
			return nil
		}
		fmt.Fprintf(stdout, "# plist path: %s\n# logs: %s and %s\n# domain: %s\n",
			resolved.PlistPath, resolved.StdOutPath, resolved.StdErrPath, resolved.Domain())
		return nil
	case "install":
		report, err := manager.Install(context.Background(), *dryRun, *force)
		fmt.Fprint(stdout, report.Describe())
		if err != nil {
			return err
		}
		if !*dryRun {
			fmt.Fprint(stdout, "\nCheck it with: codex-model-router service status\n")
		}
		return nil
	case "uninstall":
		report, err := manager.Uninstall(context.Background(), *dryRun, *force)
		fmt.Fprint(stdout, report.Describe())
		return err
	case "status":
		report, err := manager.Status(context.Background())
		fmt.Fprint(stdout, report.Describe())
		return err
	default:
		// Unreachable: the subcommand is validated above.
		return fmt.Errorf("%w: unknown service subcommand %q", errUsage, subcommand)
	}
}

// stringList collects a repeatable flag.
type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }

// Set appends, so --env may be repeated.
func (s *stringList) Set(value string) error {
	*s = append(*s, value)
	return nil
}

func parseEnvironment(pairs []string) (map[string]string, error) {
	if len(pairs) == 0 {
		return nil, nil
	}
	environment := make(map[string]string, len(pairs))
	for _, pair := range pairs {
		name, value, found := strings.Cut(pair, "=")
		if !found || name == "" {
			return nil, fmt.Errorf("--env expects KEY=VALUE, got %q", pair)
		}
		environment[name] = value
	}
	return environment, nil
}

// cmdHealthCheck probes a running router and exits non-zero when it is unhealthy.
func cmdHealthCheck(args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("healthcheck", flag.ContinueOnError)
	configPath := registerConfigFlag(flags)
	host := flags.String("host", "", "probe this host instead of the configured listen host")
	port := flags.Int("port", 0, "probe this port instead of the configured listen port")
	timeout := flags.Duration("timeout", 3*time.Second, "how long to wait for the probe")
	asJSON := flags.Bool("json", false, "print the health document as JSON")
	if err := parseFlags(flags, args); err != nil {
		return err
	}
	if flags.NArg() > 0 {
		return fmt.Errorf("%w: healthcheck takes no positional arguments", errUsage)
	}
	cfg, _, err := loadConfig(*configPath)
	if err != nil {
		return err
	}
	targetHost := *host
	if targetHost == "" {
		targetHost = cfg.Listen.Host
	}
	if targetHost == "localhost" || targetHost == "" {
		targetHost = "127.0.0.1"
	}
	targetPort := cfg.Listen.Port
	if *port > 0 {
		targetPort = *port
	}
	if targetPort <= 0 {
		return fmt.Errorf("%w: the router uses a random port, so pass --port with the value from the router's start-up log", errUsage)
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	body, err := serve.Health(ctx, targetHost, targetPort, routing.HealthPath)
	if err != nil {
		return err
	}
	if *asJSON {
		return writeJSON(stdout, body)
	}
	fmt.Fprintf(stdout, "healthy at http://%s%s: %s\n", net.JoinHostPort(targetHost, strconv.Itoa(targetPort)), routing.HealthPath, jsonCompact(body))
	return nil
}

func writeJSON(w io.Writer, value any) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

func jsonCompact(value any) string {
	data, err := json.Marshal(value)
	if err != nil {
		return "{}"
	}
	return string(data)
}

func sortStrings(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j-1] > values[j]; j-- {
			values[j-1], values[j] = values[j], values[j-1]
		}
	}
}
