// Package config loads the exporter's settings from defaults, an optional
// YAML file, environment variables and command-line flags, in that order of
// increasing precedence, and validates them.
package config

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"
)

// Defaults. The minimum interval is the server's cache lifetime: the
// endpoint renders each organization's metrics at most once a minute, so a
// faster poll returns the same body and spends the rate limit for nothing.
const (
	DefaultListenAddress = ":10056"
	DefaultBaseURL       = "https://watchfor.io"
	DefaultInterval      = 60 * time.Second
	MinInterval          = 60 * time.Second
	DefaultTimeout       = 30 * time.Second
	DefaultOrganization  = "default"
	maxOrganizationLen   = 128
	maxFilterValues      = 100
)

// Config is the whole exporter configuration.
type Config struct {
	ListenAddress string   `yaml:"listen_address"`
	Interval      Duration `yaml:"interval"`
	Timeout       Duration `yaml:"timeout"`
	Log           Log      `yaml:"log"`
	Targets       []Target `yaml:"targets"`

	// Source names where the configuration came from, for the startup log.
	Source string `yaml:"-"`
}

// Log configures the exporter's own log output (stderr).
type Log struct {
	Level  string `yaml:"level"`  // debug, info, warn, error
	Format string `yaml:"format"` // text, json
}

// Target is one WatchFor organization: one API key, one label value.
type Target struct {
	Organization string  `yaml:"organization"`
	APIKey       string  `yaml:"api_key"`
	APIKeyFile   string  `yaml:"api_key_file"`
	BaseURL      string  `yaml:"base_url"`
	Filters      Filters `yaml:"filters"`
}

// Filters narrow the per-monitor families on the server side. Organization
// totals always cover the whole organization.
type Filters struct {
	Tag          StringList `yaml:"tag"`
	Type         StringList `yaml:"type"`
	MonitorID    StringList `yaml:"monitor_id"`
	IncludeHosts *bool      `yaml:"include_hosts"`
}

// Duration is a time.Duration written as a Go duration string ("60s",
// "2m"). A bare number is refused: in YAML it would silently mean
// nanoseconds.
type Duration time.Duration

// UnmarshalYAML implements yaml.Unmarshaler.
func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.ScalarNode {
		return fmt.Errorf("line %d: expected a duration such as 60s", n.Line)
	}
	if n.Tag == "!!int" || n.Tag == "!!float" {
		return fmt.Errorf("line %d: %q has no unit; write it as %ss", n.Line, n.Value, n.Value)
	}
	v, err := time.ParseDuration(n.Value)
	if err != nil {
		return fmt.Errorf("line %d: %q is not a duration such as 60s or 2m", n.Line, n.Value)
	}
	*d = Duration(v)
	return nil
}

// StringList accepts either a YAML list or a single comma-separated string.
type StringList []string

// UnmarshalYAML implements yaml.Unmarshaler.
func (s *StringList) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		*s = splitList(n.Value)
		return nil
	case yaml.SequenceNode:
		out := make([]string, 0, len(n.Content))
		for _, c := range n.Content {
			if c.Kind != yaml.ScalarNode {
				return fmt.Errorf("line %d: expected a string", c.Line)
			}
			out = append(out, strings.TrimSpace(c.Value))
		}
		*s = out
		return nil
	default:
		return fmt.Errorf("line %d: expected a string or a list of strings", n.Line)
	}
}

func splitList(v string) []string {
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Key returns the target's API key, reading api_key_file on every call so a
// rotated Kubernetes Secret is picked up without a restart. The error never
// contains the key.
func (t *Target) Key() (string, error) {
	if t.APIKeyFile == "" {
		return t.APIKey, nil
	}
	f, err := os.Open(t.APIKeyFile)
	if err != nil {
		return "", fmt.Errorf("api_key_file: %w", err)
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 8192))
	if err != nil {
		return "", fmt.Errorf("api_key_file %s: %w", t.APIKeyFile, err)
	}
	k := string(bytes.TrimSpace(b))
	if k == "" {
		return "", fmt.Errorf("api_key_file %s is empty", t.APIKeyFile)
	}
	if strings.ContainsFunc(k, unicode.IsSpace) {
		return "", fmt.Errorf("api_key_file %s holds more than one word; it must contain only the key", t.APIKeyFile)
	}
	return k, nil
}

// MetricsURL is the native endpoint with the target's filters applied.
func (t *Target) MetricsURL() string {
	q := url.Values{}
	if len(t.Filters.Tag) > 0 {
		q["tag"] = t.Filters.Tag
	}
	if len(t.Filters.Type) > 0 {
		q["type"] = t.Filters.Type
	}
	if len(t.Filters.MonitorID) > 0 {
		q["monitor_id"] = t.Filters.MonitorID
	}
	if t.Filters.IncludeHosts != nil && !*t.Filters.IncludeHosts {
		q.Set("include_hosts", "false")
	}
	u := strings.TrimRight(t.BaseURL, "/") + "/api/v1/metrics"
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	return u
}

// Env is the environment lookup Load uses; os.LookupEnv in production.
type Env func(string) (string, bool)

// ErrHelp is returned when -h or --help was given; usage has been printed.
var ErrHelp = flag.ErrHelp

// Options are the command-line-only switches.
type Options struct {
	ShowVersion bool
}

// Load parses args (without the program name) and the environment, reads
// the YAML file if one is named, and validates the result.
func Load(args []string, env Env, usage io.Writer) (*Config, Options, error) {
	var opts Options
	fs := flag.NewFlagSet("watchfor-prometheus-exporter", flag.ContinueOnError)
	fs.SetOutput(usage)
	configFile := fs.String("config.file", "", "path to the YAML config file (env WATCHFOR_EXPORTER_CONFIG)")
	listen := fs.String("web.listen-address", DefaultListenAddress, "address to serve /metrics on (env WATCHFOR_EXPORTER_LISTEN_ADDRESS)")
	interval := fs.Duration("poll.interval", DefaultInterval, "how often each organization is polled, at least 60s (env WATCHFOR_EXPORTER_INTERVAL)")
	timeout := fs.Duration("poll.timeout", DefaultTimeout, "timeout for one poll (env WATCHFOR_EXPORTER_TIMEOUT)")
	logLevel := fs.String("log.level", "info", "debug, info, warn or error (env WATCHFOR_EXPORTER_LOG_LEVEL)")
	logFormat := fs.String("log.format", "text", "text or json (env WATCHFOR_EXPORTER_LOG_FORMAT)")
	fs.BoolVar(&opts.ShowVersion, "version", false, "print the version and exit")
	fs.Usage = func() {
		fmt.Fprintf(usage, "Usage: watchfor-prometheus-exporter [flags]\n\n")
		fmt.Fprintf(usage, "Polls the WatchFor Prometheus endpoint for one or more organizations and\nserves the merged result on /metrics.\n\nQuick start: WATCHFOR_API_KEY=wf_live_... watchfor-prometheus-exporter\n\nFlags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return nil, opts, err
	}
	if fs.NArg() > 0 {
		return nil, opts, fmt.Errorf("unexpected argument %q; settings are flags (see --help)", fs.Arg(0))
	}
	if opts.ShowVersion {
		return nil, opts, nil
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })

	cfg := &Config{
		ListenAddress: DefaultListenAddress,
		Interval:      Duration(DefaultInterval),
		Timeout:       Duration(DefaultTimeout),
		Log:           Log{Level: "info", Format: "text"},
	}

	// The YAML file.
	path := *configFile
	if !set["config.file"] {
		if v, ok := env("WATCHFOR_EXPORTER_CONFIG"); ok {
			path = v
		}
	}
	if path != "" {
		if err := readYAML(path, cfg); err != nil {
			return nil, opts, err
		}
		cfg.Source = path
	}

	// Environment.
	var errs []error
	if v, ok := env("WATCHFOR_EXPORTER_LISTEN_ADDRESS"); ok {
		cfg.ListenAddress = v
	}
	for name, dst := range map[string]*Duration{
		"WATCHFOR_EXPORTER_INTERVAL": &cfg.Interval,
		"WATCHFOR_EXPORTER_TIMEOUT":  &cfg.Timeout,
	} {
		if v, ok := env(name); ok {
			d, err := time.ParseDuration(v)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s=%q is not a duration such as 60s or 2m", name, v))
				continue
			}
			*dst = Duration(d)
		}
	}
	if v, ok := env("WATCHFOR_EXPORTER_LOG_LEVEL"); ok {
		cfg.Log.Level = v
	}
	if v, ok := env("WATCHFOR_EXPORTER_LOG_FORMAT"); ok {
		cfg.Log.Format = v
	}
	if err := quickStartTarget(cfg, env); err != nil {
		errs = append(errs, err)
	}

	// Flags.
	if set["web.listen-address"] {
		cfg.ListenAddress = *listen
	}
	if set["poll.interval"] {
		cfg.Interval = Duration(*interval)
	}
	if set["poll.timeout"] {
		cfg.Timeout = Duration(*timeout)
	}
	if set["log.level"] {
		cfg.Log.Level = *logLevel
	}
	if set["log.format"] {
		cfg.Log.Format = *logFormat
	}

	if len(errs) > 0 {
		return nil, opts, errors.Join(errs...)
	}
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, opts, err
	}
	return cfg, opts, nil
}

// quickStartTarget turns WATCHFOR_API_KEY / WATCHFOR_API_KEY_FILE into the
// single target, unless the YAML file already lists targets.
func quickStartTarget(cfg *Config, env Env) error {
	key, hasKey := env("WATCHFOR_API_KEY")
	keyFile, hasFile := env("WATCHFOR_API_KEY_FILE")
	if !hasKey && !hasFile {
		return nil
	}
	if len(cfg.Targets) > 0 {
		return errors.New("WATCHFOR_API_KEY / WATCHFOR_API_KEY_FILE are set and the config file lists targets; use one or the other")
	}
	t := Target{Organization: DefaultOrganization, APIKey: strings.TrimSpace(key), APIKeyFile: keyFile}
	if hasKey && hasFile {
		return errors.New("set WATCHFOR_API_KEY or WATCHFOR_API_KEY_FILE, not both")
	}
	if v, ok := env("WATCHFOR_ORGANIZATION"); ok {
		t.Organization = v
	}
	if v, ok := env("WATCHFOR_BASE_URL"); ok {
		t.BaseURL = v
	}
	cfg.Targets = []Target{t}
	if cfg.Source == "" {
		cfg.Source = "environment"
	}
	return nil
}

func readYAML(path string, cfg *Config) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("config file: %w", err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(cfg); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("config file %s: %w", path, err)
	}
	return nil
}

func (c *Config) applyDefaults() {
	for i := range c.Targets {
		t := &c.Targets[i]
		t.Organization = strings.TrimSpace(t.Organization)
		t.APIKey = strings.TrimSpace(t.APIKey)
		t.BaseURL = strings.TrimSpace(t.BaseURL)
		if t.BaseURL == "" {
			t.BaseURL = DefaultBaseURL
		}
		t.BaseURL = strings.TrimRight(t.BaseURL, "/")
	}
	if c.Log.Level == "" {
		c.Log.Level = "info"
	}
	if c.Log.Format == "" {
		c.Log.Format = "text"
	}
}

// Validate reports every problem at once. No message contains an API key.
func (c *Config) Validate() error {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	if c.ListenAddress == "" {
		add("listen_address is empty")
	} else if _, _, err := net.SplitHostPort(c.ListenAddress); err != nil {
		add("listen_address %q: want host:port or :port", c.ListenAddress)
	}
	iv, to := time.Duration(c.Interval), time.Duration(c.Timeout)
	if iv < MinInterval {
		add("interval %s is below the %s minimum: the WatchFor endpoint caches each organization's metrics for 60 s, so a faster poll returns the same data and spends the rate limit (30 requests/min per organization)", iv, MinInterval)
	}
	if to <= 0 {
		add("timeout must be positive, got %s", to)
	} else if iv >= MinInterval && to >= iv {
		add("timeout %s must be shorter than interval %s", to, iv)
	}
	switch c.Log.Level {
	case "debug", "info", "warn", "error":
	default:
		add("log level %q: want debug, info, warn or error", c.Log.Level)
	}
	switch c.Log.Format {
	case "text", "json":
	default:
		add("log format %q: want text or json", c.Log.Format)
	}

	if len(c.Targets) == 0 {
		add("no targets: set WATCHFOR_API_KEY (or WATCHFOR_API_KEY_FILE), or list targets in a config file (--config.file)")
	}
	seen := map[string]int{}
	for i := range c.Targets {
		t := &c.Targets[i]
		where := fmt.Sprintf("targets[%d]", i)
		if t.Organization != "" {
			where = fmt.Sprintf("targets[%d] (%s)", i, t.Organization)
		}
		for _, e := range t.validate() {
			add("%s: %w", where, e)
		}
		if t.Organization != "" {
			if j, dup := seen[t.Organization]; dup {
				add("%s: organization %q is already used by targets[%d]; every target needs its own label", where, t.Organization, j)
			}
			seen[t.Organization] = i
		}
	}
	return errors.Join(errs...)
}

func (t *Target) validate() []error {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	switch {
	case t.Organization == "":
		add("organization is empty; it becomes the organization label on every series")
	case !utf8.ValidString(t.Organization) || strings.ContainsFunc(t.Organization, unicode.IsControl):
		add("organization contains control characters or invalid UTF-8")
	case len(t.Organization) > maxOrganizationLen:
		add("organization is longer than %d bytes", maxOrganizationLen)
	}

	switch {
	case t.APIKey != "" && t.APIKeyFile != "":
		add("set api_key or api_key_file, not both")
	case t.APIKey == "" && t.APIKeyFile == "":
		add("no API key: set api_key_file (recommended) or api_key")
	case t.APIKey != "" && strings.ContainsFunc(t.APIKey, unicode.IsSpace):
		add("api_key contains whitespace")
	case t.APIKeyFile != "":
		if _, err := t.Key(); err != nil {
			errs = append(errs, err)
		}
	}

	if err := validateBaseURL(t.BaseURL); err != nil {
		errs = append(errs, err)
	}

	for _, f := range []struct {
		name string
		vals []string
	}{{"tag", t.Filters.Tag}, {"type", t.Filters.Type}, {"monitor_id", t.Filters.MonitorID}} {
		if len(f.vals) > maxFilterValues {
			add("filters.%s has %d values; at most %d", f.name, len(f.vals), maxFilterValues)
		}
		for _, v := range f.vals {
			if v == "" || strings.Contains(v, ",") || strings.ContainsFunc(v, unicode.IsControl) {
				add("filters.%s value %q: must be non-empty, without commas or control characters", f.name, v)
			}
		}
	}
	return errs
}

func validateBaseURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("base_url %q is not an absolute URL", raw)
	}
	if u.User != nil {
		return errors.New("base_url must not contain credentials; use api_key_file")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("base_url %q must not have a query or fragment", raw)
	}
	switch u.Scheme {
	case "https":
	case "http":
		// A bearer key over plain HTTP is readable by anyone on the path.
		// Loopback is allowed for local testing only.
		if !isLoopback(u.Hostname()) {
			return fmt.Errorf("base_url %q uses http; the API key would travel in clear text. Use https (http is accepted only for localhost)", raw)
		}
	default:
		return fmt.Errorf("base_url %q: scheme must be https", raw)
	}
	return nil
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
