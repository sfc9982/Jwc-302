package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func readTestConfig(t *testing.T, args []string, env map[string]string) (*config, error) {
	t.Helper()
	return readConfig(args, func(key string) string { return env[key] }, io.Discard)
}

func TestConfigFilePrecedenceAndPaths(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "config.json")
	if err := os.WriteFile(file, []byte(`{"listen_addr":":9010","iprange_file":"campus.txt","exclude_iprange_file":"vpn.txt","request_timeout":"2s","reload_interval":"0s","log_json":true,"redirect_code":307,"trusted_proxies":"::1/128"}`), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := readTestConfig(t, []string{"-config=" + file}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ListenAddr != ":9010" || cfg.IPRangeFile != filepath.Join(dir, "campus.txt") || cfg.ExcludeIPRangeFile != filepath.Join(dir, "vpn.txt") || cfg.RequestTimeout != 2*time.Second || cfg.ReloadInterval != 0 || !cfg.LogJSON || cfg.RedirectCode != 307 || len(cfg.trustedProxies) != 1 {
		t.Fatalf("file configuration: %+v", cfg)
	}
	cfg, err = readTestConfig(t, []string{"-config=" + file, "-addr=:9030", "-request-timeout=4s", "-log-json=false", "-exclude-iprange="}, map[string]string{"LISTEN_ADDR": ":9020", "REQUEST_TIMEOUT": "invalid-but-overridden", "IPRANGE_FILE": "relative-to-cwd.txt", "LOG_JSON": "invalid-but-overridden"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ListenAddr != ":9030" || cfg.RequestTimeout != 4*time.Second || cfg.LogJSON || cfg.IPRangeFile != "relative-to-cwd.txt" || cfg.ExcludeIPRangeFile != "" {
		t.Fatalf("CLI > env > file precedence: %+v", cfg)
	}
	cfg, err = readTestConfig(t, nil, map[string]string{"CONFIG_FILE": file, "REDIRECT_CODE": "302", "TRUSTED_PROXIES": "127.0.0.1/32"})
	if err != nil || cfg.RedirectCode != 302 || cfg.TrustedProxies != "127.0.0.1/32" {
		t.Fatalf("environment selection: %+v %v", cfg, err)
	}
	cfg, err = readTestConfig(t, []string{"-config="}, map[string]string{"CONFIG_FILE": "missing.json"})
	if err != nil || cfg.ListenAddr != "127.0.0.1:8080" {
		t.Fatalf("empty CLI config must disable file loading: %v", err)
	}
}

func TestRejectInvalidConfigFiles(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"unknown field", `{"request_timout":"2s"}`},
		{"duplicate field", `{"log_json":true,"log_json":false}`},
		{"null field", `{"health_path":null}`},
		{"wrong bool type", `{"log_json":"true"}`},
		{"duration number", `{"request_timeout":1000}`},
		{"bad duration", `{"request_timeout":"bad"}`},
		{"negative timeout", `{"request_timeout":"-1s"}`},
		{"array instead of object", `[]`},
		{"null instead of object", `null`},
		{"trailing data", `{} {}`},
		{"unfinished object", `{"log_json":`},
		{"oversize file", strings.Repeat(" ", (1<<20)+1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(file, []byte(tc.body), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := readTestConfig(t, []string{"-config=" + file}, nil); err == nil {
				t.Fatal("invalid config accepted")
			}
		})
	}
	if _, err := readTestConfig(t, []string{"-config=missing-file.json"}, nil); err == nil {
		t.Fatal("missing file accepted")
	}
}

func TestOperatorOptionValidation(t *testing.T) {
	for _, args := range [][]string{
		{"-redirect-code=301"}, {"-redirect-code=abc"}, {"-shutdown-timeout=0"},
		{"-ready=/health"}, {"-health=/../live"}, {"-ready=//ready"}, {"-ready=/ready//nested"}, {"-ready=/ready with space"}, {"-health=/%72eady"},
		{"-addr=127.0.0.1:65536"}, {"-addr=127.0.0.1:abc"}, {"-iprange="}, {"unexpected"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			if _, err := readTestConfig(t, args, nil); err == nil {
				t.Fatal("invalid option accepted")
			}
		})
	}
	cfg, err := readTestConfig(t, []string{"-ready=", "-health=", "-redirect-code=307", "-shutdown-timeout=3s"}, nil)
	if err != nil || cfg.ReadyPath != "" || cfg.HealthPath != "" || cfg.RedirectCode != 307 || cfg.ShutdownTimeout != 3*time.Second {
		t.Fatalf("valid operator flags: %+v %v", cfg, err)
	}
}

func TestExclusionTakesPriority(t *testing.T) {
	setTestRanges(t)
	ipRanges, _ = parseCIDRList("10.0.0.0/8,2001:db8::/32")
	excludedRanges, _ = parseCIDRList("10.20.0.0/16,2001:db8:ffff::/48")
	cfg := testConfig()
	for _, tc := range []struct {
		ip       string
		internal bool
	}{
		{"10.1.2.3", true}, {"10.20.1.2", false}, {"::ffff:10.20.1.2", false},
		{"2001:db8::1", true}, {"2001:db8:ffff::1", false}, {"198.51.100.1", false},
	} {
		t.Run(tc.ip, func(t *testing.T) {
			ctx := requestContext(tc.ip, "/course")
			makeHandler(cfg)(ctx)
			want := cfg.ExternalRedirectURL + "course"
			if tc.internal {
				want = cfg.InternalRedirectURL + "course"
			}
			if string(ctx.Response.Header.Peek("Location")) != want {
				t.Fatalf("got %s, want %s", ctx.Response.Header.Peek("Location"), want)
			}
		})
	}
}

func TestRulesReloadReadinessAndRecovery(t *testing.T) {
	setTestRanges(t)
	dir := t.TempDir()
	include := filepath.Join(dir, "include.txt")
	exclude := filepath.Join(dir, "exclude.txt")
	write := func(file, data string) {
		t.Helper()
		if err := os.WriteFile(file, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(include, "10.0.0.0/8\n")
	write(exclude, "10.20.0.0/16\n")
	if err := loadRangeFiles(include, exclude); err != nil {
		t.Fatal(err)
	}
	before := ruleState.LastSuccess
	write(include, "198.51.100.0/24\n")
	write(exclude, "invalid\n")
	if err := loadRangeFiles(include, exclude); err == nil {
		t.Fatal("bad exclusion file accepted")
	}
	if !ruleState.LastSuccess.Equal(before) || !ruleState.Failed || !containsAny(net.ParseIP("10.1.2.3"), ipRanges) || !containsAny(net.ParseIP("10.20.1.2"), excludedRanges) {
		t.Fatal("failed reload replaced part of snapshot")
	}
	cfg := testConfig()
	cfg.ReadyPath = "/ready"
	for _, tc := range []struct {
		path   string
		status int
	}{{"/health", 200}, {"/ready", 503}} {
		ctx := requestContext("127.0.0.1", tc.path)
		makeHandler(cfg)(ctx)
		var health healthResponse
		if err := json.Unmarshal(ctx.Response.Body(), &health); err != nil {
			t.Fatal(err)
		}
		if ctx.Response.StatusCode() != tc.status || !health.RulesStale || health.IPRanges != 1 || health.ExcludedRanges != 1 || health.LastReload == "" {
			t.Fatalf("%s: status %d %+v", tc.path, ctx.Response.StatusCode(), health)
		}
	}
	write(exclude, "198.51.100.0/25\n")
	if err := loadRangeFiles(include, exclude); err != nil {
		t.Fatal(err)
	}
	ctx := requestContext("127.0.0.1", "/ready")
	makeHandler(cfg)(ctx)
	if ctx.Response.StatusCode() != 200 || ruleState.Failed || containsAny(net.ParseIP("10.1.2.3"), ipRanges) {
		t.Fatal("successful reload did not recover readiness and replace snapshot")
	}
}

func Test307PreservesPOSTAcrossRedirect(t *testing.T) {
	setTestRanges(t)
	ipRanges, _ = parseCIDRList("127.0.0.1/32")
	calls := make(chan string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		calls <- r.Method + " " + r.URL.RequestURI() + " " + string(body)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	cfg := testConfig()
	cfg.RedirectCode = 307
	cfg.InternalRedirectURL = upstream.URL + "/jsxsd/"
	gateway, client := startHTTPGateway(t, cfg)
	client.CheckRedirect = nil
	resp, err := client.Post(gateway+"/submit?a=1", "application/x-www-form-urlencoded", strings.NewReader("course=42"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 204 {
		t.Fatalf("final status: %d", resp.StatusCode)
	}
	select {
	case got := <-calls:
		if got != "POST /jsxsd/submit?a=1 course=42" {
			t.Fatalf("method/body/query changed: %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("redirect did not reach upstream")
	}
}

func TestQueryLoggingIsOptIn(t *testing.T) {
	setTestRanges(t)
	for _, enabled := range []bool{false, true} {
		var logs bytes.Buffer
		logger = slog.New(slog.NewJSONHandler(&logs, nil))
		cfg := testConfig()
		cfg.LogQuery = enabled
		ctx := requestContext("10.1.2.3", "/course?ticket=private-value&x=1")
		makeHandler(cfg)(ctx)
		if !strings.Contains(string(ctx.Response.Header.Peek("Location")), "ticket=private-value") {
			t.Fatal("log policy altered actual redirect query")
		}
		if got := strings.Contains(logs.String(), "private-value"); got != enabled {
			t.Fatalf("LogQuery=%v: leaked=%v: %s", enabled, got, logs.String())
		}
		var entry map[string]any
		if err := json.Unmarshal(logs.Bytes(), &entry); err != nil {
			t.Fatal(err)
		}
		if !enabled && (strings.Contains(entry["uri"].(string), "?") || strings.Contains(entry["target"].(string), "?")) {
			t.Fatal("query delimiter retained in default logs")
		}
	}
}

func TestReloaderStopsWithContext(t *testing.T) {
	setTestRanges(t)
	cfg := testConfig()
	cfg.ReloadInterval = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	done := startReloader(ctx, cfg)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("reloader did not stop")
	}
}

func TestReadyOnlyUpstreamChecks(t *testing.T) {
	setTestRanges(t)
	calls := make(chan struct{}, 2)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls <- struct{}{}; w.WriteHeader(503) }))
	defer upstream.Close()
	cfg := testConfig()
	cfg.ReadyPath = "/ready"
	cfg.ReadyCheckUpstream = true
	cfg.InternalRedirectURL, cfg.ExternalRedirectURL = upstream.URL, upstream.URL
	live := requestContext("127.0.0.1", "/health")
	makeHandler(cfg)(live)
	if live.Response.StatusCode() != 200 || len(calls) != 0 {
		t.Fatal("liveness must not probe upstreams with ready-check-upstream")
	}
	ready := requestContext("127.0.0.1", "/ready")
	makeHandler(cfg)(ready)
	if ready.Response.StatusCode() != 503 || len(calls) != 2 {
		t.Fatalf("readiness must check both upstreams: status=%d probes=%d", ready.Response.StatusCode(), len(calls))
	}
}

func TestHealthRoutesRejectUnsafeOriginPaths(t *testing.T) {
	setTestRanges(t)
	cfg := testConfig()
	cfg.ReadyPath = "/ready"
	for _, uri := range []string{"/x/../health", "/%2e%2e/ready", "/a%5C..%5Chealth", "//health", "/health#fragment"} {
		t.Run(uri, func(t *testing.T) {
			ctx := requestContext("127.0.0.1", uri)
			makeHandler(cfg)(ctx)
			if ctx.Response.StatusCode() != 400 || len(ctx.Response.Header.Peek("Location")) != 0 {
				t.Fatalf("unsafe URI intercepted as valid endpoint: %d", ctx.Response.StatusCode())
			}
		})
	}
}
