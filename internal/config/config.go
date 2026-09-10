// Package config loads and validates the router's JSON configuration.
//
// The configuration is deliberately explicit: every upstream, model ID, and
// header policy that the router uses must be written down here. Requests
// cannot select an upstream on their own, which is what keeps the process from
// becoming an open proxy.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Default sizes and addresses used when the configuration omits a value.
const (
	DefaultListenHost     = "127.0.0.1"
	DefaultListenPort     = 4317
	DefaultBasePath       = "/v1"
	DefaultMaxRequestByte = int64(32 << 20)

	// ChatGPTBackendBaseURL is the account-session Responses endpoint. Requests
	// that carry ChatGPT-Account-ID are sent here instead of the API base URL.
	ChatGPTBackendBaseURL = "https://chatgpt.com/backend-api/codex"
	// OpenAIAPIBaseURL is the API-key Responses endpoint.
	OpenAIAPIBaseURL = "https://api.openai.com/v1"
)

// Adapter names accepted by Route.Reasoning.Adapter.
const (
	AdapterNone               = "none"
	AdapterSGLangChatTemplate = "sglang_chat_template"
)

// Item policy names accepted by Route.Input.
const (
	PolicyDrop   = "drop"
	PolicyKeep   = "keep"
	PolicyReject = "reject"
	PolicyError  = "error"
	PolicyMap    = "map_to_function_calls"
)

// Config is the whole router configuration.
type Config struct {
	Listen Listen `json:"listen"`
	// BasePath is the URL prefix Codex appends paths to. The generated
	// openai_base_url value is http://<host>:<port><BasePath>.
	BasePath string `json:"base_path"`
	// MaxRequestBytes bounds decoded request bodies, in every direction that
	// the router itself reads.
	MaxRequestBytes int64   `json:"max_request_bytes"`
	Log             Log     `json:"log"`
	Native          Native  `json:"native"`
	Routes          []Route `json:"routes"`
	Catalog         Catalog `json:"catalog"`

	// baseDir is the directory of the file this configuration was loaded from. It is
	// empty for bytes that were not read from a file, and relative catalog paths are
	// resolved against it.
	baseDir string

	// routeByModel maps an exact model ID to its remote route. It is built by
	// Validate and is the only routing lookup the handler performs.
	routeByModel map[string]*Route
	// nativeModels is the exact set of model IDs allowed to reach the native
	// upstreams.
	nativeModels map[string]bool
}

// Listen describes the loopback-only listener.
type Listen struct {
	Host string `json:"host"`
	Port int    `json:"port"`
}

// Log controls output. Nothing here logs request bodies or headers.
type Log struct {
	// Level is one of debug, info, warn, or error.
	Level string `json:"level"`
	// Requests logs a one-line summary per routed request. It is off by
	// default so a long-running LaunchAgent does not grow log files forever.
	Requests bool `json:"requests"`
}

// Native describes the trusted upstreams used for native model IDs.
type Native struct {
	ChatGPTBaseURL string `json:"chatgpt_base_url"`
	APIBaseURL     string `json:"api_base_url"`
	// PreserveClientAuth forwards the incoming Authorization and account
	// headers to the native upstreams only. It never applies to remote routes.
	PreserveClientAuth *bool `json:"preserve_client_auth"`
	// APIKeyEnv names an environment variable holding a key for APIBaseURL.
	// It is used when a request has no ChatGPT account header.
	APIKeyEnv string `json:"api_key_env"`
	// Models are additional native model IDs allowed through. The catalog
	// file's slugs are allowed as well.
	Models []string `json:"models"`
}

// Route is one self-hosted OpenAI-compatible upstream.
type Route struct {
	Name      string            `json:"name"`
	BaseURL   string            `json:"base_url"`
	Models    []string          `json:"models"`
	Auth      *Auth             `json:"auth,omitempty"`
	Headers   map[string]string `json:"extra_headers"`
	Reasoning Reasoning         `json:"reasoning"`
	Input     Input             `json:"input"`
	// ResponseHeaderTimeoutSeconds bounds the wait for upstream response
	// headers. Zero uses the default; -1 disables the bound. Generation time is
	// never bounded, because headers arrive before the model finishes.
	ResponseHeaderTimeoutSeconds int `json:"response_header_timeout_seconds"`
}

// Auth is the route's own credential, taken from a configured environment
// variable. Incoming credentials are never forwarded to a remote route.
type Auth struct {
	// APIKeyEnv names the environment variable that holds the key.
	APIKeyEnv string `json:"api_key_env"`
	// Header is the destination header. Defaults to Authorization.
	Header string `json:"header"`
	// Scheme is "bearer" or "header". "bearer" prefixes the value with
	// "Bearer "; "header" sends the raw value.
	Scheme string `json:"scheme"`
}

// Reasoning selects how Codex reasoning requests are translated.
type Reasoning struct {
	Adapter string `json:"adapter"`
	// SupportedEfforts optionally restricts the efforts the adapter maps.
	SupportedEfforts []string `json:"supported_efforts"`
	// UnknownEffort is "drop" (default, use the server default) or "error".
	UnknownEffort string `json:"unknown_effort"`
	// ForwardReasoning keeps the Codex reasoning object for the upstream.
	ForwardReasoning bool `json:"forward_reasoning"`
	// ChatTemplateKwargs is used by AdapterSGLangChatTemplate. A value that is
	// a JSON object is treated as a per-effort map; any other value is sent
	// literally. A null effort value omits the key for that effort.
	ChatTemplateKwargs map[string]json.RawMessage `json:"chat_template_kwargs"`
}

// Input controls how Codex conversation items are translated for a route.
type Input struct {
	// ReasoningItems handles encrypted provider reasoning state. Default drop.
	ReasoningItems string `json:"reasoning_items"`
	// CompactionItems handles encrypted compaction state. Default drop.
	CompactionItems string `json:"compaction_items"`
	// CustomTools handles free-form custom tool calls and outputs. Default
	// map_to_function_calls.
	CustomTools string `json:"custom_tools"`
	// DeveloperRoleAsSystem rewrites developer messages to system role.
	DeveloperRoleAsSystem *bool `json:"developer_role_as_system"`
	// UnknownItems handles input item types the router does not model.
	// Default drop, which prevents sending unmodelled state to a server that
	// cannot validate it.
	UnknownItems string `json:"unknown_items"`
}

// Catalog configures combined model catalog generation.
type Catalog struct {
	// NativeCatalogFile points at a raw Codex model catalog JSON file, for
	// example the output of `codex debug models --bundled`.
	NativeCatalogFile string `json:"native_catalog_file"`
	// OutputFile is where the combined catalog is written. The path is stored
	// in Codex's model_catalog_json key.
	OutputFile string `json:"output_file"`
	// Models are the self-hosted entries added to the picker.
	Models []CatalogModel `json:"models"`
	// NativeModelIDsFromCatalog keeps catalog slugs in the native allow list.
	// Default true.
	NativeModelIDsFromCatalog *bool `json:"native_model_ids_from_catalog"`
	// BaseInstructionsFile supplies base_instructions for local entries. When
	// empty the first non-empty native catalog value is reused.
	BaseInstructionsFile string `json:"base_instructions_file"`
	// Description is the picker description for local entries.
	Description string `json:"description"`
}

// CatalogModel is one self-hosted picker entry.
type CatalogModel struct {
	ID              string   `json:"id"`
	Route           string   `json:"route"`
	DisplayName     string   `json:"display_name"`
	Description     string   `json:"description"`
	ContextWindow   int      `json:"context_window"`
	InputModalities []string `json:"input_modalities"`
	// ReasoningLevels are advertised in the picker. An empty list means the
	// model has no selectable thinking levels.
	ReasoningLevels []string `json:"reasoning_levels"`
	// DefaultReasoningLevel must appear in ReasoningLevels when both are set.
	DefaultReasoningLevel string                     `json:"default_reasoning_level"`
	ToolCapable           bool                       `json:"tool_capable"`
	Priority              int                        `json:"priority"`
	BaseInstructions      string                     `json:"base_instructions"`
	ExperimentalExtra     map[string]json.RawMessage `json:"extra_fields"`
}

// resolvePath expands ~ and makes a path absolute against the directory the
// configuration was loaded from. An absolute path is returned unchanged, and a path
// in a configuration that was never loaded from a file stays as written.
func (c *Config) resolvePath(path string) string {
	path = expandHome(strings.TrimSpace(path))
	if path == "" || filepath.IsAbs(path) || c.baseDir == "" {
		return path
	}
	return filepath.Join(c.baseDir, path)
}

// ErrInvalid marks a configuration problem. A caller can map it to a distinct exit
// code without matching on message text.
var ErrInvalid = errors.New("invalid configuration")

// Load reads, decodes, and validates a configuration file. The path is included in
// every error because several configurations usually exist on one machine.
//
// Relative paths inside the file are resolved against the directory that holds it,
// so a configuration copied between machines keeps pointing at its own catalog and
// instructions files no matter where the process was started.
func Load(path string) (*Config, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("%w: resolve %s: %w", ErrInvalid, path, err)
	}
	data, err := os.ReadFile(absolute)
	if err != nil {
		return nil, fmt.Errorf("%w: read %s: %w", ErrInvalid, absolute, err)
	}
	cfg, err := parseInDirectory(data, filepath.Dir(absolute))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", absolute, err)
	}
	return cfg, nil
}

// Parse decodes and validates configuration bytes with no base directory, which is
// how the tests and the embedded example are parsed. Relative paths are then
// interpreted by whoever uses them.
func Parse(data []byte) (*Config, error) {
	return parseInDirectory(data, "")
}

// parseInDirectory is the shared decode path. Unknown fields are an error so a typo
// cannot silently disable a security-relevant setting. directory, when set, is the
// base for relative paths in the file.
func parseInDirectory(data []byte, directory string) (*Config, error) {
	if len(strings.TrimSpace(string(data))) == 0 {
		return nil, fmt.Errorf("%w: the configuration is empty", ErrInvalid)
	}
	cfg := &Config{}
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(cfg); err != nil {
		return nil, fmt.Errorf("%w: decode config: %w", ErrInvalid, err)
	}
	if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, fmt.Errorf("%w: the configuration contains more than one JSON value", ErrInvalid)
		}
		return nil, fmt.Errorf("%w: decode config: %w", ErrInvalid, err)
	}
	cfg.baseDir = directory
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Validate checks every field and compiles the routing tables.
// applyDefaults fills omitted values before validation. Fields that a user
// must opt into are never defaulted to a permissive value.
func (c *Config) applyDefaults() {
	if c.MaxRequestBytes == 0 {
		c.MaxRequestBytes = DefaultMaxRequestByte
	}
}

func (c *Config) Validate() error {
	var problems []string
	add := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}

	// The compiled tables are filled by the normalize steps below, so they must
	// exist before those steps run.
	c.routeByModel = make(map[string]*Route)
	c.nativeModels = make(map[string]bool)

	if err := c.normalizeListen(); err != nil {
		add("%v", err)
	}
	if err := c.normalizeBasePath(); err != nil {
		add("%v", err)
	}
	if c.MaxRequestBytes <= 0 {
		add("max_request_bytes must be positive")
	}
	if err := normalizeLogLevel(&c.Log.Level); err != nil {
		add("%v", err)
	}
	if err := c.normalizeNative(); err != nil {
		add("%v", err)
	}

	seenRoutes := make(map[string]bool, len(c.Routes))
	for i := range c.Routes {
		route := &c.Routes[i]
		if err := route.validate(); err != nil {
			add("routes[%d]: %v", i, err)
			continue
		}
		if seenRoutes[route.Name] {
			add("routes[%d]: duplicate route name %q", i, route.Name)
			continue
		}
		seenRoutes[route.Name] = true
		for _, model := range route.Models {
			if existing, ok := c.routeByModel[model]; ok {
				add("routes[%d]: model %q is already routed by route %q", i, model, existing.Name)
				continue
			}
			c.routeByModel[model] = route
		}
	}

	if err := c.normalizeCatalog(seenRoutes); err != nil {
		add("%v", err)
	}

	if len(c.routeByModel) == 0 && len(c.nativeModels) == 0 {
		add("no models are configured: declare at least one route or native.model")
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return fmt.Errorf("%w:\n  - %s", ErrInvalid, strings.Join(problems, "\n  - "))
	}
	return nil
}

// RouteFor returns the remote route for an exact model ID.
// RouteFor returns the remote route registered for an exact model ID. The lookup
// is exact: a model ID with surrounding whitespace is not normalised here, so it
// cannot accidentally match a configured ID.
func (c *Config) RouteFor(model string) (*Route, bool) {
	route, ok := c.routeByModel[model]
	return route, ok
}

// IsNativeModel reports whether a model ID may reach the native upstreams.
// Anything outside this set is rejected instead of forwarded.
// IsNativeModel reports whether an exact model ID may reach the native upstreams.
// Anything outside the configured set is refused rather than forwarded.
func (c *Config) IsNativeModel(model string) bool {
	return c.nativeModels[model]
}

// RoutedModelIDs returns the sorted set of model IDs served by remote routes.
func (c *Config) RoutedModelIDs() []string {
	ids := make([]string, 0, len(c.routeByModel))
	for id := range c.routeByModel {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// NativeModelIDs returns the sorted set of model IDs allowed natively.
func (c *Config) NativeModelIDs() []string {
	ids := make([]string, 0, len(c.nativeModels))
	for id := range c.nativeModels {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// PreserveClientAuth reports whether native requests may carry client auth.
func (c *Config) PreserveClientAuth() bool {
	return c.Native.PreserveClientAuth == nil || *c.Native.PreserveClientAuth
}

// CatalogUsesNativeModelIDs reports whether catalog slugs join the native
// allow list.
func (c *Config) CatalogUsesNativeModelIDs() bool {
	return c.Catalog.NativeModelIDsFromCatalog == nil || *c.Catalog.NativeModelIDsFromCatalog
}

// DeveloperRoleAsSystem reports the route's developer-role policy.
func (r *Route) DeveloperRoleAsSystem() bool {
	return r.Input.DeveloperRoleAsSystem == nil || *r.Input.DeveloperRoleAsSystem
}

// Addr returns the listener address for the validated configuration.
func (c *Config) Addr() string {
	return net.JoinHostPort(c.Listen.Host, strconv.Itoa(c.Listen.Port))
}

// BaseURL is the value for Codex's openai_base_url. After a listener is open, the
// bound address is reported instead, which matters when the port was 0.
func (c *Config) BaseURL() string {
	return (&url.URL{Scheme: "http", Host: c.Addr(), Path: c.BasePath}).String()
}

func (c *Config) normalizeListen() error {
	host := strings.TrimSpace(c.Listen.Host)
	if host == "" {
		host = DefaultListenHost
	}
	if host == "localhost" {
		c.Listen.Host = host
		return nil
	}
	if strings.ContainsAny(host, "/?# \t") {
		return fmt.Errorf("listen.host %q is not a bare host", host)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("listen.host %q must be localhost, a loopback address, or ::1", host)
	}
	if !ip.IsLoopback() {
		return fmt.Errorf("listen.host %q is not a loopback address; the router must not be reachable from other machines", host)
	}
	c.Listen.Host = ip.String()
	return nil
}

func (c *Config) normalizeBasePath() error {
	path := strings.TrimSpace(c.BasePath)
	if path == "" {
		c.BasePath = DefaultBasePath
		return nil
	}
	if !strings.HasPrefix(path, "/") {
		return fmt.Errorf("base_path %q must start with /", path)
	}
	if strings.Contains(path, "?") || strings.Contains(path, "#") || strings.Contains(path, "..") {
		return fmt.Errorf("base_path %q must be a plain path", path)
	}
	c.BasePath = strings.TrimRight(path, "/")
	if c.BasePath == "" {
		c.BasePath = "/"
	}
	return nil
}

func normalizeLogLevel(level *string) error {
	value := strings.ToLower(strings.TrimSpace(*level))
	if value == "" {
		*level = "warn"
		return nil
	}
	switch value {
	case "debug", "info", "warn", "error":
		*level = value
		return nil
	default:
		return fmt.Errorf("log.level %q must be debug, info, warn, or error", *level)
	}
}

func (c *Config) normalizeNative() error {
	native := &c.Native
	if native.PreserveClientAuth == nil {
		enabled := true
		native.PreserveClientAuth = &enabled
	}
	if strings.TrimSpace(native.ChatGPTBaseURL) == "" {
		native.ChatGPTBaseURL = ChatGPTBackendBaseURL
	}
	if strings.TrimSpace(native.APIBaseURL) == "" {
		native.APIBaseURL = OpenAIAPIBaseURL
	}
	for _, field := range []struct {
		name  string
		value *string
	}{{"native.chatgpt_base_url", &native.ChatGPTBaseURL}, {"native.api_base_url", &native.APIBaseURL}} {
		normalized, err := normalizeUpstreamURL(*field.value)
		if err != nil {
			return fmt.Errorf("%s: %w", field.name, err)
		}
		if *native.PreserveClientAuth && !strings.HasPrefix(normalized, "https://") && !isLoopbackHostURL(normalized) {
			return fmt.Errorf("%s: preserve_client_auth requires https, got %q", field.name, *field.value)
		}
		*field.value = normalized
	}
	if native.APIKeyEnv != "" && !validEnvName(native.APIKeyEnv) {
		return fmt.Errorf("native.api_key_env %q is not a valid environment name", native.APIKeyEnv)
	}
	seen := make(map[string]bool, len(native.Models))
	models := make([]string, 0, len(native.Models))
	for _, model := range native.Models {
		model = strings.TrimSpace(model)
		if model == "" || seen[model] {
			continue
		}
		seen[model] = true
		models = append(models, model)
		c.nativeModels[model] = true
	}
	native.Models = models
	return nil
}

func (c *Config) normalizeCatalog(seenRoutes map[string]bool) error {
	catalog := &c.Catalog
	catalog.NativeCatalogFile = c.resolvePath(catalog.NativeCatalogFile)
	catalog.OutputFile = c.resolvePath(catalog.OutputFile)
	catalog.BaseInstructionsFile = c.resolvePath(catalog.BaseInstructionsFile)
	seen := make(map[string]bool, len(catalog.Models))
	for i := range catalog.Models {
		model := &catalog.Models[i]
		model.ID = strings.TrimSpace(model.ID)
		model.Route = strings.TrimSpace(model.Route)
		switch {
		case model.ID == "":
			return fmt.Errorf("catalog.models[%d]: id is required", i)
		case model.Route == "":
			return fmt.Errorf("catalog.models[%d]: route is required", i)
		case !seenRoutes[model.Route]:
			return fmt.Errorf("catalog.models[%d]: route %q is not defined", i, model.Route)
		}
		if seen[model.ID] {
			return fmt.Errorf("catalog.models[%d]: duplicate id %q", i, model.ID)
		}
		seen[model.ID] = true
		if native := strings.TrimSpace(model.ID); c.nativeModels[native] {
			return fmt.Errorf("catalog.models[%d]: id %q is also declared in native.models", i, model.ID)
		}
		if model.ContextWindow < 0 {
			return fmt.Errorf("catalog.models[%d]: context_window must not be negative", i)
		}
		if model.DefaultReasoningLevel != "" && !containsString(model.ReasoningLevels, model.DefaultReasoningLevel) {
			return fmt.Errorf("catalog.models[%d]: default_reasoning_level %q is not in reasoning_levels", i, model.DefaultReasoningLevel)
		}
		for _, modality := range model.InputModalities {
			if strings.TrimSpace(modality) == "" {
				return fmt.Errorf("catalog.models[%d]: input_modalities contains an empty value", i)
			}
		}
		if _, ok := c.routeByModel[model.ID]; !ok {
			route := c.routeByName(model.Route)
			if route == nil {
				return fmt.Errorf("catalog.models[%d]: route %q vanished", i, model.Route)
			}
			route.Models = append(route.Models, model.ID)
			c.routeByModel[model.ID] = route
		}
	}
	if catalog.NativeCatalogFile != "" && c.CatalogUsesNativeModelIDs() {
		ids, err := catalogSlugs(catalog.NativeCatalogFile)
		if err != nil {
			return fmt.Errorf("catalog.native_catalog_file: %w", err)
		}
		for _, id := range ids {
			if _, clash := c.routeByModel[id]; clash {
				return fmt.Errorf("catalog.native_catalog_file: native model %q is also routed remotely", id)
			}
			c.nativeModels[id] = true
		}
	}
	return nil
}

func (c *Config) routeByName(name string) *Route {
	for i := range c.Routes {
		if c.Routes[i].Name == name {
			return &c.Routes[i]
		}
	}
	return nil
}

// validate checks a single route definition.
func (r *Route) validate() error {
	r.Name = strings.TrimSpace(r.Name)
	if r.Name == "" {
		return errors.New("name is required")
	}
	if !validRouteName(r.Name) {
		return fmt.Errorf("name %q must use letters, digits, dot, dash, or underscore", r.Name)
	}
	base, err := normalizeUpstreamURL(r.BaseURL)
	if err != nil {
		return fmt.Errorf("base_url: %w", err)
	}
	r.BaseURL = base

	seen := make(map[string]bool, len(r.Models))
	models := make([]string, 0, len(r.Models))
	for _, model := range r.Models {
		model = strings.TrimSpace(model)
		if model == "" || seen[model] {
			continue
		}
		if strings.ContainsAny(model, " \t\r\n") {
			return fmt.Errorf("models contains %q with whitespace; model IDs must be exact", model)
		}
		seen[model] = true
		models = append(models, model)
	}
	if len(models) == 0 {
		return errors.New("at least one model ID is required")
	}
	r.Models = models

	if r.Auth != nil {
		if !validEnvName(r.Auth.APIKeyEnv) {
			return errors.New("auth.api_key_env must name an environment variable")
		}
		switch strings.ToLower(strings.TrimSpace(r.Auth.Scheme)) {
		case "", "bearer":
			r.Auth.Scheme = "bearer"
		case "header":
			r.Auth.Scheme = "header"
		default:
			return fmt.Errorf("auth.scheme %q must be bearer or header", r.Auth.Scheme)
		}
		header := strings.TrimSpace(r.Auth.Header)
		if header == "" {
			header = "Authorization"
		}
		if err := validCredentialHeader(header); err != nil {
			return fmt.Errorf("auth.header: %w", err)
		}
		r.Auth.Header = http.CanonicalHeaderKey(header)
		if r.Auth.Header == "Authorization" && r.Auth.Scheme == "header" {
			return errors.New("auth.scheme header cannot be used with Authorization; use bearer")
		}
	}
	for name, value := range r.Headers {
		if err := validOutboundHeader(name); err != nil {
			return fmt.Errorf("extra_headers: %w", err)
		}
		if strings.ContainsAny(value, "\r\n") {
			return fmt.Errorf("extra_headers[%q] must not contain CR or LF", name)
		}
	}

	switch strings.TrimSpace(r.Reasoning.Adapter) {
	case "":
		r.Reasoning.Adapter = AdapterNone
	case AdapterNone:
		r.Reasoning.ChatTemplateKwargs = nil
	case AdapterSGLangChatTemplate:
		if len(r.Reasoning.ChatTemplateKwargs) == 0 {
			return errors.New("reasoning.chat_template_kwargs is required for the sglang_chat_template adapter")
		}
		if err := validateEffortMaps(r.Reasoning.ChatTemplateKwargs); err != nil {
			return fmt.Errorf("reasoning.chat_template_kwargs: %w", err)
		}
	default:
		return fmt.Errorf("reasoning.adapter %q is unknown", r.Reasoning.Adapter)
	}
	if err := normalizeEffortPolicy(&r.Reasoning.UnknownEffort); err != nil {
		return fmt.Errorf("reasoning.unknown_effort: %w", err)
	}

	if err := normalizePolicy(&r.Input.ReasoningItems, PolicyDrop, PolicyKeep, PolicyReject); err != nil {
		return fmt.Errorf("input.reasoning_items: %w", err)
	}
	if err := normalizePolicy(&r.Input.CompactionItems, PolicyDrop, PolicyReject); err != nil {
		return fmt.Errorf("input.compaction_items: %w", err)
	}
	if err := normalizePolicy(&r.Input.CustomTools, PolicyMap, PolicyKeep, PolicyDrop, PolicyReject); err != nil {
		return fmt.Errorf("input.custom_tools: %w", err)
	}
	if err := normalizePolicy(&r.Input.UnknownItems, PolicyDrop, PolicyKeep, PolicyReject); err != nil {
		return fmt.Errorf("input.unknown_items: %w", err)
	}
	if r.Input.DeveloperRoleAsSystem == nil {
		enabled := true
		r.Input.DeveloperRoleAsSystem = &enabled
	}
	if r.ResponseHeaderTimeoutSeconds < -1 {
		return errors.New("response_header_timeout_seconds must be -1, 0, or positive")
	}
	return nil
}

// blockedTransportHeaders are never set from configuration or copied from the
// incoming request. They either belong to the hop, name the caller's identity, or
// select a destination.
var blockedTransportHeaders = map[string]bool{
	"Host":                true,
	"Content-Length":      true,
	"Transfer-Encoding":   true,
	"Connection":          true,
	"Upgrade":             true,
	"Te":                  true,
	"Trailer":             true,
	"Cookie":              true,
	"Cookie2":             true,
	"Proxy-Authorization": true,
	"Forwarded":           true,
	"X-Forwarded":         true,
	"X-Forwarded-For":     true,
	"X-Forwarded-Host":    true,
	"X-Forwarded-Proto":   true,
	"Chatgpt-Account-Id":  true,
	"X-Openai-Intent":     true,
	"X-Client-Request-Id": true,
	"X-Request-Id":        true,
}

// forbiddenOutboundHeaders additionally blocks Authorization as a plain
// extra_header value: a route credential goes through route.auth, which reads it
// from a configured environment variable rather than from request data.
var forbiddenOutboundHeaders = map[string]bool{
	"Authorization": true,
	"X-Api-Key":     true,
}

func init() {
	for name := range blockedTransportHeaders {
		forbiddenOutboundHeaders[name] = true
	}
}

// allowedRequestHeaders may be copied from an incoming request to a remote
// route. Everything else, including every credential and account header, is
// dropped.
var allowedRequestHeaders = []string{"Accept", "Content-Type", "Openai-Beta", "User-Agent"}

// AllowedRequestHeaders returns the copied-header allow list.
func AllowedRequestHeaders() []string {
	return append([]string(nil), allowedRequestHeaders...)
}

// validCredentialHeader checks the header that carries a route's own key.
// Authorization and provider-specific headers are fine; transport, forwarding, and
// account headers are not.
func validCredentialHeader(name string) error {
	trimmed := strings.TrimSpace(name)
	if err := validHeaderName(trimmed); err != nil {
		return fmt.Errorf("header name %q is %w", name, err)
	}
	if blockedTransportHeaders[http.CanonicalHeaderKey(trimmed)] {
		return fmt.Errorf("header %q cannot carry a route credential", name)
	}
	return nil
}

// validOutboundHeader checks a configured extra_headers name. Route credentials do
// not belong here, so Authorization is refused.
func validOutboundHeader(name string) error {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return errors.New("header name is empty")
	}
	if err := validHeaderName(trimmed); err != nil {
		return fmt.Errorf("header name %q is %w", name, err)
	}
	if forbiddenOutboundHeaders[http.CanonicalHeaderKey(trimmed)] {
		return fmt.Errorf("header %q may not be set from extra_headers", name)
	}
	return nil
}

func validHeaderName(name string) error {
	if name == "" {
		return errors.New("empty")
	}
	for _, character := range name {
		if character >= 0x80 || character == ' ' || character == '\t' || character < 0x21 || strings.ContainsRune(":\"(),/<=>?@[]{}\\\x7f", character) {
			return errors.New("invalid")
		}
	}
	return nil
}

func validRouteName(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-' || r == '_' || r == '.':
		default:
			return false
		}
	}
	return true
}

func validEnvName(name string) bool {
	if name == "" || strings.ContainsAny(name, "=\r\n ") {
		return false
	}
	for i, r := range name {
		if r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r == '_' {
			continue
		}
		if r >= '0' && r <= '9' && i > 0 {
			continue
		}
		return false
	}
	return true
}

// normalizeEffortPolicy accepts the two effort policies: "drop", which lets the
// server use its own default, and "error", which refuses the request.
func normalizeEffortPolicy(value *string) error {
	trimmed := strings.ToLower(strings.TrimSpace(*value))
	if trimmed == "" {
		*value = PolicyDrop
		return nil
	}
	switch trimmed {
	case PolicyDrop, PolicyError:
		*value = trimmed
		return nil
	default:
		return fmt.Errorf("must be drop or error")
	}
}

func normalizePolicy(value *string, allowed ...string) error {
	trimmed := strings.ToLower(strings.TrimSpace(*value))
	if trimmed == "" {
		*value = allowed[0]
		return nil
	}
	for _, option := range allowed {
		if trimmed == option {
			*value = option
			return nil
		}
	}
	return fmt.Errorf("must be one of %s", strings.Join(allowed, ", "))
}

// normalizeUpstreamURL trims and rejects URLs that could smuggle credentials or
// select a target through their path query.
func normalizeUpstreamURL(raw string) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", errors.New("is empty")
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return "", fmt.Errorf("invalid URL %q: %w", raw, err)
	}
	switch parsed.Scheme {
	case "http", "https":
	case "":
		return "", fmt.Errorf("URL %q must include a scheme", raw)
	default:
		return "", fmt.Errorf("URL %q has unsupported scheme %q", raw, parsed.Scheme)
	}
	if parsed.User != nil {
		return "", fmt.Errorf("URL %q must not embed credentials", raw)
	}
	if parsed.Host == "" {
		return "", fmt.Errorf("URL %q must include a host", raw)
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("URL %q must not include a query or fragment", raw)
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	parsed.RawPath = ""
	return parsed.String(), nil
}

func isLoopbackHostURL(raw string) bool {
	parsed, err := url.Parse(raw)
	if err != nil {
		return false
	}
	host := parsed.Hostname()
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func expandHome(path string) string {
	if path == "~" || strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(path, "~"))
		}
	}
	return path
}
