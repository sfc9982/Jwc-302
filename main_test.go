package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/valyala/fasthttp"
)

func TestBuildTarget(t *testing.T) {
	const base = "https://jwc.example/jsxsd/"
	for _, tc := range []struct{ uri, want string }{
		{"/", base},
		{"/page?foo=1&foo=2", base + "page?foo=1&foo=2"},
		{"/?next=https%3A%2F%2Fexample.com", base + "?next=https%3A%2F%2Fexample.com"},
		{"/jsxsd/page?q=%2F", base + "page?q=%2F"},
		{"/jsxsd", base},
		{"/jsxsd/", base},
		{"/jsxsd-other/page", base + "jsxsd-other/page"},
		{"/https://evil.example/page", base + "https://evil.example/page"},
		{"/a%2Fb/%E4%B8%AD%E6%96%87?q=hello+world", base + "a%2Fb/%E4%B8%AD%E6%96%87?q=hello+world"},
	} {
		t.Run(tc.uri, func(t *testing.T) {
			got, err := buildTarget(base, tc.uri)
			if err != nil || got != tc.want {
				t.Fatalf("got %q, %v; want %q", got, err, tc.want)
			}
		})
	}
	for _, uri := range []string{"/../admin", "/a/./b", "/%2e%2e/admin", "/a%2F..%2Fadmin", "/a%5Cb", "//evil.example/page", "///evil.example", "https://evil.example", "/bad%zz", "/page#fragment"} {
		if got, err := buildTarget(base, uri); err == nil {
			t.Errorf("accepted %q as %q", uri, got)
		}
	}
}

func testConfig() *config {
	return &config{
		ListenAddr: "127.0.0.1:8080", InternalRedirectURL: "http://internal.example/jsxsd/",
		ExternalRedirectURL: "https://external.example/jsxsd/", HealthPath: "/health", RequestTimeout: time.Second, ShutdownTimeout: time.Second, RedirectCode: 302, IPRangeFile: "iprange.txt",
	}
}

func TestValidateConfig(t *testing.T) {
	if err := validateConfig(testConfig()); err != nil {
		t.Fatal(err)
	}
	for _, base := range []string{"", "ftp://example.com/", "/relative/", "http:///", "http://user:pass@example.com/", "https://example.com/?q=1", "https://example.com/#", "https://example.com/a/%2e%2e/", "https://example.com/a%5Cb/"} {
		cfg := testConfig()
		cfg.InternalRedirectURL = normalizeBaseURL(base)
		if err := validateConfig(cfg); err == nil {
			t.Errorf("accepted invalid base %q", base)
		}
	}
	for _, mutate := range []func(*config){
		func(c *config) { c.RequestTimeout = 0 },
		func(c *config) { c.RequestTimeout = -time.Second },
		func(c *config) { c.ReloadInterval = -time.Second },
		func(c *config) { c.ListenAddr = "invalid" },
		func(c *config) { c.HealthPath = "health" },
		func(c *config) { c.HealthPath = "/health?x=1" },
	} {
		cfg := testConfig()
		mutate(cfg)
		if err := validateConfig(cfg); err == nil {
			t.Error("accepted invalid configuration")
		}
	}
}

func requestContext(ip, uri string) *fasthttp.RequestCtx {
	var req fasthttp.Request
	req.SetRequestURI(uri)
	ctx := &fasthttp.RequestCtx{}
	ctx.Init(&req, &net.TCPAddr{IP: net.ParseIP(ip), Port: 12345}, nil)
	return ctx
}

func TestClientIP(t *testing.T) {
	trusted, err := parseCIDRList("127.0.0.1/32,10.0.0.0/8")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ direct, xff, xri, want string }{
		{"203.0.113.1", "10.1.1.1", "10.1.1.1", "203.0.113.1"},
		{"127.0.0.1", "198.51.100.1, 10.1.1.1", "10.2.2.2", "198.51.100.1"},
		{"127.0.0.1", "10.2.2.2, 198.51.100.1", "", "198.51.100.1"},
		{"127.0.0.1", "10.2.2.2, malformed", "10.3.3.3", "127.0.0.1"},
		{"127.0.0.1", "10.2.2.2, 10.3.3.3", "", "10.2.2.2"},
		{"127.0.0.1", "", "198.51.100.2", "198.51.100.2"},
		{"127.0.0.1", "", "invalid", "127.0.0.1"},
	} {
		ctx := requestContext(tc.direct, "/")
		ctx.Request.Header.Set("X-Forwarded-For", tc.xff)
		ctx.Request.Header.Set("X-Real-IP", tc.xri)
		if got := clientIP(ctx, trusted).String(); got != tc.want {
			t.Errorf("%+v: got %s", tc, got)
		}
	}
}

func setTestRanges(t *testing.T) {
	t.Helper()
	old := ipRanges
	oldExcluded, oldState := excludedRanges, ruleState
	excludedRanges = nil
	ruleState.LastSuccess = time.Time{}
	ruleState.Failed = false
	oldLogger := logger
	logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	ipRanges, _ = parseCIDRList("10.0.0.0/8")
	t.Cleanup(func() { ipRanges = old; excludedRanges = oldExcluded; ruleState = oldState; logger = oldLogger })
}

func TestRedirectHandler(t *testing.T) {
	setTestRanges(t)
	logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := testConfig()
	for _, tc := range []struct {
		ip, uri, target string
		status          int
	}{
		{"10.1.2.3", "/page?x=1", cfg.InternalRedirectURL + "page?x=1", 302},
		{"203.0.113.1", "/jsxsd/page?x=1", cfg.ExternalRedirectURL + "page?x=1", 302},
		{"203.0.113.1", "/https://evil.example/page", cfg.ExternalRedirectURL + "https://evil.example/page", 302},
		{"203.0.113.1", "/%2e%2e/admin", "", 400},
	} {
		ctx := requestContext(tc.ip, tc.uri)
		ctx.Request.Header.Set("X-Real-IP", "10.1.2.3")
		makeHandler(cfg)(ctx)
		if ctx.Response.StatusCode() != tc.status || string(ctx.Response.Header.Peek("Location")) != tc.target {
			t.Errorf("%+v: got %d, %q", tc, ctx.Response.StatusCode(), ctx.Response.Header.Peek("Location"))
		}
	}
}

func TestReloadRetainsLastValidRanges(t *testing.T) {
	setTestRanges(t)
	file := filepath.Join(t.TempDir(), "iprange.txt")
	if err := os.WriteFile(file, []byte("192.0.2.0/24\ninvalid\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := loadIPRanges(file); err == nil || !containsAny(net.ParseIP("10.1.2.3"), ipRanges) {
		t.Fatal("failed reload replaced existing ranges")
	}
	if err := os.WriteFile(file, []byte("# comment\n\n192.0.2.0/24\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := loadIPRanges(file); err != nil || !containsAny(net.ParseIP("192.0.2.1"), ipRanges) || containsAny(net.ParseIP("10.1.2.3"), ipRanges) {
		t.Fatal("valid reload did not replace ranges", err)
	}
}

func TestHealth(t *testing.T) {
	for _, tc := range []struct {
		name           string
		upstreamStatus int
		probe, slow    bool
		want           int
		state          string
	}{
		{"liveness", 500, false, false, 200, "ok"},
		{"ready", 200, true, false, 200, "ok"},
		{"failed", 500, true, false, 503, "degraded"},
		{"timeout", 200, true, true, 503, "degraded"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.slow {
					time.Sleep(600 * time.Millisecond)
				}
				w.WriteHeader(tc.upstreamStatus)
			}))
			defer server.Close()
			cfg := testConfig()
			cfg.CheckUpstream = tc.probe
			cfg.InternalRedirectURL, cfg.ExternalRedirectURL = server.URL, server.URL
			cfg.RequestTimeout = 800 * time.Millisecond
			ctx := requestContext("127.0.0.1", "/health")
			start := time.Now()
			makeHandler(cfg)(ctx)
			elapsed := time.Since(start)
			var response healthResponse
			if err := json.Unmarshal(ctx.Response.Body(), &response); err != nil {
				t.Fatal(err)
			}
			if ctx.Response.StatusCode() != tc.want || response.Status != tc.state {
				t.Fatalf("got %d %+v", ctx.Response.StatusCode(), response)
			}
			if tc.slow && elapsed >= cfg.RequestTimeout {
				t.Fatalf("health checks exceeded response budget: %s", elapsed)
			}
			if !tc.probe && response.Upstream != nil {
				t.Fatal("liveness performed upstream checks")
			}
		})
	}
}

func TestConfigCLI(t *testing.T) {
	if os.Getenv("JWC_TEST_CONFIG") == "1" {
		os.Args = append([]string{os.Args[0]}, strings.Split(os.Getenv("JWC_TEST_FLAGS"), " ")...)
		cfg := parseConfig()
		if cfg.LogJSON || cfg.ListenAddr != "127.0.0.1:9000" {
			os.Exit(2)
		}
		os.Exit(0)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestConfigCLI$")
	cmd.Env = gatewayTestEnv("JWC_TEST_CONFIG=1", "JWC_TEST_FLAGS=-log-json=false -addr=127.0.0.1:9000", "LOG_JSON=true", "LISTEN_ADDR=:8081")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("CLI override failed: %v\n%s", err, out)
	}
}
