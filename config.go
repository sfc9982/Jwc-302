package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type config struct {
	ListenAddr          string
	IPRangeFile         string
	ExcludeIPRangeFile  string
	InternalRedirectURL string
	ExternalRedirectURL string
	HealthPath          string
	ReadyPath           string
	ReloadInterval      time.Duration
	RequestTimeout      time.Duration
	ShutdownTimeout     time.Duration
	CheckUpstream       bool
	ReadyCheckUpstream  bool
	LogJSON             bool
	LogQuery            bool
	RedirectCode        int
	TrustedProxies      string
	CheckConfig         bool
	trustedProxies      []*net.IPNet
}

func defaultConfig() *config {
	return &config{
		ListenAddr: "127.0.0.1:8080", IPRangeFile: "iprange.txt",
		InternalRedirectURL: "https://internal.example.edu/jsxsd/", ExternalRedirectURL: "https://vpn.example.edu/jsxsd/",
		HealthPath: "/health", ReadyPath: "/ready", ReloadInterval: 5 * time.Minute,
		RequestTimeout: 5 * time.Second, ShutdownTimeout: 10 * time.Second, RedirectCode: 302,
	}
}

type configOption struct {
	name, env, help string
	value           any
}

func configOptions(c *config) []configOption {
	return []configOption{
		{"addr", "LISTEN_ADDR", "listen address", &c.ListenAddr},
		{"iprange", "IPRANGE_FILE", "campus CIDR file", &c.IPRangeFile},
		{"exclude-iprange", "EXCLUDE_IPRANGE_FILE", "optional CIDR file forced to the external entry", &c.ExcludeIPRangeFile},
		{"internal", "INTERNAL_URL", "internal redirect base URL", &c.InternalRedirectURL},
		{"external", "EXTERNAL_URL", "external redirect base URL", &c.ExternalRedirectURL},
		{"health", "HEALTH_PATH", "health endpoint (empty to disable)", &c.HealthPath},
		{"ready", "READY_PATH", "readiness endpoint (empty to disable)", &c.ReadyPath},
		{"trusted-proxies", "TRUSTED_PROXIES", "trusted proxy CIDRs, comma separated", &c.TrustedProxies},
		{"reload-interval", "RELOAD_INTERVAL", "CIDR reload interval (0 to disable)", &c.ReloadInterval},
		{"request-timeout", "REQUEST_TIMEOUT", "request and upstream timeout budget", &c.RequestTimeout},
		{"shutdown-timeout", "SHUTDOWN_TIMEOUT", "graceful shutdown deadline", &c.ShutdownTimeout},
		{"check-upstream", "CHECK_UPSTREAM", "probe upstreams on health and readiness requests", &c.CheckUpstream},
		{"ready-check-upstream", "READY_CHECK_UPSTREAM", "probe upstreams on readiness requests only", &c.ReadyCheckUpstream},
		{"log-json", "LOG_JSON", "write JSON logs", &c.LogJSON},
		{"log-query", "LOG_QUERY", "include query parameters in request logs", &c.LogQuery},
		{"redirect-code", "REDIRECT_CODE", "redirect status: 302 or 307", &c.RedirectCode},
	}
}

func (o configOption) set(s string) error {
	switch value := o.value.(type) {
	case *string:
		*value = s
	case *time.Duration:
		v, err := time.ParseDuration(s)
		if err != nil {
			return err
		}
		*value = v
	case *bool:
		switch strings.ToLower(s) {
		case "1", "true", "yes", "on":
			*value = true
		case "0", "false", "no", "off":
			*value = false
		default:
			return fmt.Errorf("expected bool")
		}
	case *int:
		v, err := strconv.Atoi(s)
		if err != nil {
			return err
		}
		*value = v
	default:
		return fmt.Errorf("unsupported option type")
	}
	return nil
}

func (o configOption) setJSON(raw json.RawMessage) error {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return fmt.Errorf("null is not a configuration value")
	}
	if _, duration := o.value.(*time.Duration); duration {
		var text string
		if err := json.Unmarshal(raw, &text); err != nil {
			return fmt.Errorf("duration must be a string: %w", err)
		}
		return o.set(text)
	}
	return json.Unmarshal(raw, o.value)
}

// Parse CLI first. Environment values explicitly overridden by a CLI flag
// are not parsed, so malformed shadowed environment values cannot block startup.
func readConfig(args []string, getenv func(string) string, output io.Writer) (*config, error) {
	cli := defaultConfig()
	flags := flag.NewFlagSet("jwc-302", flag.ContinueOnError)
	flags.SetOutput(output)
	configFile := flags.String("config", getenv("CONFIG_FILE"), "optional JSON configuration file")
	checkConfig := flags.Bool("check-config", false, "validate configuration and CIDR files, then exit")
	for _, option := range configOptions(cli) {
		switch value := option.value.(type) {
		case *string:
			flags.StringVar(value, option.name, *value, option.help)
		case *time.Duration:
			flags.DurationVar(value, option.name, *value, option.help)
		case *bool:
			flags.BoolVar(value, option.name, *value, option.help)
		case *int:
			flags.IntVar(value, option.name, *value, option.help)
		}
	}
	if err := flags.Parse(args); err != nil {
		return nil, err
	}
	if flags.NArg() != 0 {
		return nil, fmt.Errorf("unexpected positional arguments: %s", strings.Join(flags.Args(), " "))
	}
	visited := map[string]bool{}
	flags.Visit(func(f *flag.Flag) { visited[f.Name] = true })
	cfg := defaultConfig()
	options := configOptions(cfg)
	if *configFile != "" {
		if err := applyConfigFile(*configFile, options); err != nil {
			return nil, fmt.Errorf("invalid config file: %w", err)
		}
	}
	cliOptions := configOptions(cli)
	for i, option := range options {
		if visited[option.name] {
			// Marshal native CLI values to reuse the same typed assignment. Duration is
			// represented as text in JSON instead of its nanosecond integer value.
			var raw []byte
			if value, ok := cliOptions[i].value.(*time.Duration); ok {
				raw, _ = json.Marshal(value.String())
			} else {
				raw, _ = json.Marshal(cliOptions[i].value)
			}
			if err := option.setJSON(raw); err != nil {
				return nil, err
			}
		} else if text := getenv(option.env); text != "" {
			if err := option.set(text); err != nil {
				return nil, fmt.Errorf("invalid %s: %w", option.env, err)
			}
		}
	}
	cfg.CheckConfig = *checkConfig
	cfg.InternalRedirectURL = normalizeBaseURL(cfg.InternalRedirectURL)
	cfg.ExternalRedirectURL = normalizeBaseURL(cfg.ExternalRedirectURL)
	trusted, err := parseCIDRList(cfg.TrustedProxies)
	if err != nil {
		return nil, fmt.Errorf("invalid trusted-proxies: %w", err)
	}
	cfg.trustedProxies = trusted
	if err := validateConfig(cfg); err != nil {
		return nil, fmt.Errorf("invalid configuration: %w", err)
	}
	return cfg, nil
}

func applyConfigFile(filename string, options []configOption) error {
	file, err := os.Open(filename)
	if err != nil {
		return err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, (1<<20)+1))
	if err != nil {
		return err
	}
	if len(data) > 1<<20 {
		return fmt.Errorf("configuration file exceeds 1 MiB")
	}
	// Token-level decoding rejects duplicate keys as well as unknown fields.
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return fmt.Errorf("expected a JSON object")
	}
	byKey := map[string]configOption{}
	for _, option := range options {
		byKey[strings.ToLower(option.env)] = option
	}
	seen := map[string]bool{}
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		key, ok := token.(string)
		if !ok {
			return fmt.Errorf("expected a JSON key")
		}
		option, known := byKey[key]
		if !known {
			return fmt.Errorf("unknown configuration key %q", key)
		}
		if seen[key] {
			return fmt.Errorf("duplicate configuration key %q", key)
		}
		seen[key] = true
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			return err
		}
		if err := option.setJSON(raw); err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
		if key == "iprange_file" || key == "exclude_iprange_file" {
			value := option.value.(*string)
			if *value != "" && !filepath.IsAbs(*value) {
				*value = filepath.Join(filepath.Dir(filename), *value)
			}
		}
	}
	if _, err := decoder.Token(); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return fmt.Errorf("unexpected data after configuration object")
	}
	return nil
}

func parseConfig() *config {
	cfg, err := readConfig(os.Args[1:], os.Getenv, os.Stderr)
	if errors.Is(err, flag.ErrHelp) {
		os.Exit(0)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	return cfg
}

func validateConfig(cfg *config) error {
	if cfg.RequestTimeout <= 0 || cfg.ShutdownTimeout <= 0 || cfg.ReloadInterval < 0 {
		return fmt.Errorf("request-timeout and shutdown-timeout must be positive; reload-interval must be non-negative")
	}
	if _, port, err := net.SplitHostPort(cfg.ListenAddr); err != nil {
		return fmt.Errorf("invalid listen address: %w", err)
	} else if p, err := strconv.Atoi(port); err != nil || p < 0 || p > 65535 {
		return fmt.Errorf("invalid listen port")
	}
	if cfg.IPRangeFile == "" {
		return fmt.Errorf("iprange file must not be empty")
	}
	if cfg.RedirectCode != 302 && cfg.RedirectCode != 307 {
		return fmt.Errorf("redirect-code must be 302 or 307")
	}
	for _, endpoint := range []string{cfg.HealthPath, cfg.ReadyPath} {
		if endpoint == "" {
			continue
		}
		if !strings.HasPrefix(endpoint, "/") || strings.HasPrefix(endpoint, "//") || strings.ContainsAny(endpoint, "?#%\\ \t\r\n") || unsafePath(endpoint) {
			return fmt.Errorf("health and ready paths must be plain absolute paths without traversal, query or fragment")
		}
		canonical := path.Clean(endpoint)
		if endpoint != canonical && endpoint != canonical+"/" {
			return fmt.Errorf("endpoint path must not contain repeated slashes")
		}
		if _, err := url.ParseRequestURI(endpoint); err != nil {
			return fmt.Errorf("invalid endpoint path: %w", err)
		}
	}
	if cfg.HealthPath != "" && cfg.HealthPath == cfg.ReadyPath {
		return fmt.Errorf("health and ready paths must differ")
	}
	for _, base := range []string{cfg.InternalRedirectURL, cfg.ExternalRedirectURL} {
		u, err := url.Parse(base)
		if err != nil {
			return fmt.Errorf("invalid redirect URL: %w", err)
		}
		if (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(base, "#") || unsafePath(u.Path) {
			return fmt.Errorf("redirect URL must be an absolute HTTP(S) URL without credentials, query, fragment or traversal")
		}
	}
	return nil
}

func normalizeBaseURL(s string) string {
	s = strings.TrimSpace(s)
	if s != "" && !strings.HasSuffix(s, "/") {
		s += "/"
	}
	return s
}
