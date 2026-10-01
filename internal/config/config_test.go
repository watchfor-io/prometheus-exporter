package config

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const secret = "wf_live_s3cr3t_do_not_print"

func envOf(m map[string]string) Env {
	return func(k string) (string, bool) {
		v, ok := m[k]
		return v, ok
	}
}

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	keyFile := writeFile(t, dir, "key", "  "+secret+"\n")
	emptyKey := writeFile(t, dir, "empty", "\n\n")
	twoWords := writeFile(t, dir, "two", secret+" extra\n")

	cases := []struct {
		name    string
		yaml    string // written to a file and passed with --config.file when set
		args    []string
		env     map[string]string
		wantErr []string // substrings; empty means success
		check   func(t *testing.T, c *Config)
	}{
		{
			name: "env quick start",
			env:  map[string]string{"WATCHFOR_API_KEY": secret},
			check: func(t *testing.T, c *Config) {
				if len(c.Targets) != 1 {
					t.Fatalf("%d targets", len(c.Targets))
				}
				tg := c.Targets[0]
				if tg.Organization != "default" || tg.BaseURL != DefaultBaseURL || tg.APIKey != secret {
					t.Errorf("target = %+v", tg)
				}
				if c.ListenAddress != ":10056" || time.Duration(c.Interval) != time.Minute || time.Duration(c.Timeout) != 30*time.Second {
					t.Errorf("defaults = %s %s %s", c.ListenAddress, time.Duration(c.Interval), time.Duration(c.Timeout))
				}
			},
		},
		{
			name: "env quick start with key file, organization and base URL",
			env: map[string]string{
				"WATCHFOR_API_KEY_FILE": keyFile,
				"WATCHFOR_ORGANIZATION": "acme",
				"WATCHFOR_BASE_URL":     "https://staging.watchfor.example/",
			},
			check: func(t *testing.T, c *Config) {
				tg := c.Targets[0]
				if tg.Organization != "acme" || tg.BaseURL != "https://staging.watchfor.example" {
					t.Errorf("target = %+v", tg)
				}
				if k, err := tg.Key(); err != nil || k != secret {
					t.Errorf("Key() = %q, %v", k, err)
				}
			},
		},
		{
			name:    "nothing configured",
			wantErr: []string{"no targets: set WATCHFOR_API_KEY"},
		},
		{
			name:    "both env key and env key file",
			env:     map[string]string{"WATCHFOR_API_KEY": secret, "WATCHFOR_API_KEY_FILE": keyFile},
			wantErr: []string{"not both"},
		},
		{
			name: "full YAML",
			yaml: `
listen_address: "127.0.0.1:9999"
interval: 2m
timeout: 20s
log: {level: debug, format: json}
targets:
  - organization: acme
    api_key_file: ` + keyFile + `
    filters:
      tag: [prod, web]
      type: http,ssl
      include_hosts: false
  - organization: globex
    api_key: ` + secret + `
    base_url: http://localhost:3000
    filters:
      monitor_id: 0fe0d9ba-c062-48f5-a9ec-ead9a4076ebd
`,
			check: func(t *testing.T, c *Config) {
				if c.ListenAddress != "127.0.0.1:9999" || time.Duration(c.Interval) != 2*time.Minute || c.Log.Format != "json" {
					t.Errorf("config = %+v", c)
				}
				a := c.Targets[0]
				if got := a.MetricsURL(); got != "https://watchfor.io/api/v1/metrics?include_hosts=false&tag=prod&tag=web&type=http&type=ssl" {
					t.Errorf("acme URL = %s", got)
				}
				if got := c.Targets[1].MetricsURL(); got != "http://localhost:3000/api/v1/metrics?monitor_id=0fe0d9ba-c062-48f5-a9ec-ead9a4076ebd" {
					t.Errorf("globex URL = %s", got)
				}
			},
		},
		{
			name: "precedence: flags over env over YAML",
			yaml: "interval: 5m\nlisten_address: ':1'\ntargets: [{organization: a, api_key: k}]\n",
			env:  map[string]string{"WATCHFOR_EXPORTER_INTERVAL": "3m", "WATCHFOR_EXPORTER_LISTEN_ADDRESS": ":2"},
			args: []string{"--poll.interval=4m"},
			check: func(t *testing.T, c *Config) {
				if time.Duration(c.Interval) != 4*time.Minute {
					t.Errorf("interval = %s, want the flag's 4m", time.Duration(c.Interval))
				}
				if c.ListenAddress != ":2" {
					t.Errorf("listen = %s, want the env's :2", c.ListenAddress)
				}
			},
		},
		{
			name:    "interval below the minimum is refused with the reason",
			env:     map[string]string{"WATCHFOR_API_KEY": secret},
			args:    []string{"--poll.interval=30s"},
			wantErr: []string{"interval 30s is below the 1m0s minimum", "caches each organization's metrics for 60 s"},
		},
		{
			name:    "interval below the minimum in YAML",
			yaml:    "interval: 15s\ntargets: [{organization: a, api_key: k}]\n",
			wantErr: []string{"interval 15s is below"},
		},
		{
			name:    "timeout not shorter than the interval",
			env:     map[string]string{"WATCHFOR_API_KEY": secret},
			args:    []string{"--poll.timeout=60s"},
			wantErr: []string{"timeout 1m0s must be shorter than interval 1m0s"},
		},
		{
			name:    "bare number duration",
			yaml:    "interval: 60\ntargets: [{organization: a, api_key: k}]\n",
			wantErr: []string{`"60" has no unit; write it as 60s`},
		},
		{
			name:    "bad env duration",
			env:     map[string]string{"WATCHFOR_API_KEY": secret, "WATCHFOR_EXPORTER_INTERVAL": "soon"},
			wantErr: []string{`WATCHFOR_EXPORTER_INTERVAL="soon" is not a duration`},
		},
		{
			name:    "unknown YAML field",
			yaml:    "targets: [{organization: a, api_key: k, apikey: typo}]\n",
			wantErr: []string{"field apikey not found"},
		},
		{
			name:    "env key and YAML targets together",
			yaml:    "targets: [{organization: a, api_key: k}]\n",
			env:     map[string]string{"WATCHFOR_API_KEY": secret},
			wantErr: []string{"use one or the other"},
		},
		{
			name:    "missing organization",
			yaml:    "targets: [{api_key: k}]\n",
			wantErr: []string{"targets[0]: organization is empty"},
		},
		{
			name:    "duplicate organization",
			yaml:    "targets: [{organization: a, api_key: k}, {organization: a, api_key: k2}]\n",
			wantErr: []string{`organization "a" is already used by targets[0]`},
		},
		{
			name:    "both key and key file",
			yaml:    "targets: [{organization: a, api_key: " + secret + ", api_key_file: " + keyFile + "}]\n",
			wantErr: []string{"set api_key or api_key_file, not both"},
		},
		{
			name:    "no key",
			yaml:    "targets: [{organization: a}]\n",
			wantErr: []string{"no API key"},
		},
		{
			name:    "missing key file",
			yaml:    "targets: [{organization: a, api_key_file: " + filepath.Join(dir, "nope") + "}]\n",
			wantErr: []string{"api_key_file", "no such file"},
		},
		{
			name:    "empty key file",
			yaml:    "targets: [{organization: a, api_key_file: " + emptyKey + "}]\n",
			wantErr: []string{"is empty"},
		},
		{
			name:    "key file with two words",
			yaml:    "targets: [{organization: a, api_key_file: " + twoWords + "}]\n",
			wantErr: []string{"more than one word"},
		},
		{
			name:    "plain http to a public host",
			yaml:    "targets: [{organization: a, api_key: " + secret + ", base_url: 'http://watchfor.io'}]\n",
			wantErr: []string{"uses http; the API key would travel in clear text"},
		},
		{
			name: "plain http to loopback is allowed",
			yaml: "targets: [{organization: a, api_key: k, base_url: 'http://127.0.0.1:8080'}]\n",
		},
		{
			name:    "base URL with credentials",
			yaml:    "targets: [{organization: a, api_key: k, base_url: 'https://u:p@watchfor.io'}]\n",
			wantErr: []string{"must not contain credentials"},
		},
		{
			name:    "base URL with query",
			yaml:    "targets: [{organization: a, api_key: k, base_url: 'https://watchfor.io/?x=1'}]\n",
			wantErr: []string{"must not have a query"},
		},
		{
			name:    "base URL not absolute",
			yaml:    "targets: [{organization: a, api_key: k, base_url: 'watchfor.io'}]\n",
			wantErr: []string{"is not an absolute URL"},
		},
		{
			name:    "filter value with a comma inside a list",
			yaml:    "targets: [{organization: a, api_key: k, filters: {tag: ['a,b']}}]\n",
			wantErr: []string{"filters.tag value"},
		},
		{
			name:    "bad listen address",
			env:     map[string]string{"WATCHFOR_API_KEY": secret},
			args:    []string{"--web.listen-address=10056"},
			wantErr: []string{"want host:port or :port"},
		},
		{
			name:    "bad log level and format",
			env:     map[string]string{"WATCHFOR_API_KEY": secret, "WATCHFOR_EXPORTER_LOG_LEVEL": "loud"},
			args:    []string{"--log.format=xml"},
			wantErr: []string{`log level "loud"`, `log format "xml"`},
		},
		{
			name:    "stray argument",
			env:     map[string]string{"WATCHFOR_API_KEY": secret},
			args:    []string{"run"},
			wantErr: []string{`unexpected argument "run"`},
		},
		{
			name: "config file from env",
			env:  map[string]string{"WATCHFOR_EXPORTER_CONFIG": "<yaml>"},
			yaml: "targets: [{organization: from-env-file, api_key: k}]\n",
			check: func(t *testing.T, c *Config) {
				if c.Targets[0].Organization != "from-env-file" {
					t.Errorf("targets = %+v", c.Targets)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string(nil), tc.args...)
			env := map[string]string{}
			for k, v := range tc.env {
				env[k] = v
			}
			if tc.yaml != "" {
				p := writeFile(t, t.TempDir(), "config.yml", tc.yaml)
				if env["WATCHFOR_EXPORTER_CONFIG"] == "<yaml>" {
					env["WATCHFOR_EXPORTER_CONFIG"] = p
				} else {
					args = append([]string{"--config.file=" + p}, args...)
				}
			}
			cfg, _, err := Load(args, envOf(env), io.Discard)
			if len(tc.wantErr) == 0 {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if tc.check != nil {
					tc.check(t, cfg)
				}
				return
			}
			if err == nil {
				t.Fatalf("want an error containing %q", tc.wantErr)
			}
			for _, w := range tc.wantErr {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q does not contain %q", err, w)
				}
			}
			if strings.Contains(err.Error(), secret) {
				t.Errorf("error leaks the API key: %q", err)
			}
		})
	}
}

func TestVersionFlag(t *testing.T) {
	cfg, opts, err := Load([]string{"--version"}, envOf(nil), io.Discard)
	if err != nil || !opts.ShowVersion || cfg != nil {
		t.Fatalf("got cfg=%v opts=%+v err=%v", cfg, opts, err)
	}
}

func TestHelpFlag(t *testing.T) {
	var out strings.Builder
	_, _, err := Load([]string{"--help"}, envOf(nil), &out)
	if err != ErrHelp {
		t.Fatalf("err = %v, want ErrHelp", err)
	}
	if !strings.Contains(out.String(), "--poll.interval") && !strings.Contains(out.String(), "-poll.interval") {
		t.Errorf("usage lacks the flags:\n%s", out.String())
	}
}

func TestKeyFileRotation(t *testing.T) {
	p := writeFile(t, t.TempDir(), "key", "first\n")
	tg := Target{APIKeyFile: p}
	if k, _ := tg.Key(); k != "first" {
		t.Fatalf("Key() = %q", k)
	}
	writeFile(t, filepath.Dir(p), "key", "second\n")
	if k, _ := tg.Key(); k != "second" {
		t.Fatalf("Key() after rotation = %q", k)
	}
}

func TestStringListScalar(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "c.yml", "targets: [{organization: a, api_key: k, filters: {tag: ' prod , , web '}}]\n")
	cfg, _, err := Load([]string{"--config.file=" + p}, envOf(nil), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(cfg.Targets[0].Filters.Tag, "|")
	if got != "prod|web" {
		t.Errorf("tags = %q, want prod|web", got)
	}
}

// TestExampleConfig keeps examples/config.example.yml loadable.
func TestExampleConfig(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "examples", "config.example.yml"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	key := writeFile(t, dir, "key", secret+"\n")
	text := strings.NewReplacer(
		"/run/secrets/watchfor-acme-staging", key,
		"/run/secrets/watchfor-acme", key,
	).Replace(string(b))
	p := writeFile(t, dir, "config.yml", text)
	cfg, _, err := Load([]string{"--config.file=" + p}, envOf(nil), io.Discard)
	if err != nil {
		t.Fatalf("examples/config.example.yml does not load: %v", err)
	}
	if len(cfg.Targets) != 2 || cfg.Targets[1].Filters.IncludeHosts == nil || *cfg.Targets[1].Filters.IncludeHosts {
		t.Errorf("unexpected targets: %+v", cfg.Targets)
	}
}
