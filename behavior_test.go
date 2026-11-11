package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/valyala/fasthttp"
)

// Child processes must not inherit a developer's gateway configuration.
func gatewayTestEnv(extra ...string) []string {
	keys := map[string]bool{}
	for _, key := range []string{"LISTEN_ADDR", "IPRANGE_FILE", "INTERNAL_URL", "EXTERNAL_URL", "HEALTH_PATH", "TRUSTED_PROXIES", "RELOAD_INTERVAL", "REQUEST_TIMEOUT", "CHECK_UPSTREAM", "LOG_JSON", "JWC_TEST_CONFIG", "JWC_TEST_FLAGS", "JWC_CONFIG_PROBE", "JWC_CONFIG_ARGS"} {
		keys[key] = true
	}
	for _, value := range extra {
		key, _, _ := strings.Cut(value, "=")
		keys[key] = true
	}
	var env []string
	for _, value := range os.Environ() {
		key, _, _ := strings.Cut(value, "=")
		if !keys[strings.ToUpper(key)] {
			env = append(env, value)
		}
	}
	return append(env, extra...)
}

func TestConfigProbe(t *testing.T) {
	if os.Getenv("JWC_CONFIG_PROBE") != "1" {
		return
	}
	var args []string
	if err := json.Unmarshal([]byte(os.Getenv("JWC_CONFIG_ARGS")), &args); err != nil {
		panic(err)
	}
	os.Args = append([]string{os.Args[0]}, args...)
	cfg := parseConfig()
	if err := json.NewEncoder(os.Stdout).Encode(struct {
		*config
		TrustedCount int
	}{cfg, len(cfg.trustedProxies)}); err != nil {
		panic(err)
	}
	os.Exit(0)
}

func TestConfigurationBehavior(t *testing.T) {
	for _, tc := range []struct {
		name      string
		env, args []string
		errorText string
		verify    func(*testing.T, *config, int)
	}{
		{name: "defaults", verify: func(t *testing.T, c *config, n int) {
			if c.ListenAddr != "127.0.0.1:8080" || c.ReloadInterval != 5*time.Minute || c.RequestTimeout != 5*time.Second || c.LogJSON || c.CheckUpstream || n != 0 {
				t.Fatalf("unexpected defaults: %+v, proxies=%d", c, n)
			}
		}},
		{name: "environment", env: []string{"LISTEN_ADDR=:9001", "INTERNAL_URL=https://inside.example/app", "EXTERNAL_URL=https://outside.example/app", "TRUSTED_PROXIES=127.0.0.1/32,::1/128", "RELOAD_INTERVAL=0", "REQUEST_TIMEOUT=2s", "CHECK_UPSTREAM=yes", "LOG_JSON=on", "HEALTH_PATH=/ready"}, verify: func(t *testing.T, c *config, n int) {
			if c.ListenAddr != ":9001" || c.InternalRedirectURL != "https://inside.example/app/" || c.ExternalRedirectURL != "https://outside.example/app/" || c.HealthPath != "/ready" || c.ReloadInterval != 0 || c.RequestTimeout != 2*time.Second || !c.CheckUpstream || !c.LogJSON || n != 2 {
				t.Fatalf("environment not applied: %+v, proxies=%d", c, n)
			}
		}},
		{name: "flags override environment", env: []string{"LISTEN_ADDR=:9001", "RELOAD_INTERVAL=1m", "REQUEST_TIMEOUT=2s", "CHECK_UPSTREAM=true", "LOG_JSON=true", "HEALTH_PATH=/ready"}, args: []string{"-addr=:9002", "-reload-interval=0", "-request-timeout=3s", "-check-upstream=false", "-log-json=false", "-health="}, verify: func(t *testing.T, c *config, _ int) {
			if c.ListenAddr != ":9002" || c.ReloadInterval != 0 || c.RequestTimeout != 3*time.Second || c.CheckUpstream || c.LogJSON || c.HealthPath != "" {
				t.Fatalf("flags did not override environment: %+v", c)
			}
		}},
		{name: "invalid duration", env: []string{"REQUEST_TIMEOUT=oops"}, errorText: "invalid REQUEST_TIMEOUT"},
		{name: "invalid reload", env: []string{"RELOAD_INTERVAL=oops"}, errorText: "invalid RELOAD_INTERVAL"},
		{name: "invalid bool", env: []string{"CHECK_UPSTREAM=maybe"}, errorText: "invalid CHECK_UPSTREAM"},
		{name: "invalid logging", env: []string{"LOG_JSON=maybe"}, errorText: "invalid LOG_JSON"},
		{name: "invalid trusted proxy", args: []string{"-trusted-proxies=not-an-ip"}, errorText: "invalid trusted-proxies"},
		{name: "zero timeout", args: []string{"-request-timeout=0"}, errorText: "invalid configuration"},
		{name: "negative reload", args: []string{"-reload-interval=-1s"}, errorText: "invalid configuration"},
		{name: "unsafe destination", args: []string{"-external=https://user:pass@example.com/"}, errorText: "invalid configuration"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args, _ := json.Marshal(tc.args)
			cmd := exec.Command(os.Args[0], "-test.run=^TestConfigProbe$")
			cmd.Env = gatewayTestEnv(append(tc.env, "JWC_CONFIG_PROBE=1", "JWC_CONFIG_ARGS="+string(args))...)
			out, err := cmd.CombinedOutput()
			if tc.errorText != "" {
				if err == nil || !strings.Contains(string(out), tc.errorText) {
					t.Fatalf("want failure containing %q; got %v: %s", tc.errorText, err, out)
				}
				return
			}
			if err != nil {
				t.Fatalf("parse config: %v: %s", err, out)
			}
			var result struct {
				config
				TrustedCount int
			}
			if err := json.Unmarshal(out, &result); err != nil {
				t.Fatalf("decode config: %v: %s", err, out)
			}
			tc.verify(t, &result.config, result.TrustedCount)
		})
	}
}

func TestCIDRMembership(t *testing.T) {
	networks, err := parseCIDRList(" 10.0.0.0/8, ,2001:db8::/32,192.0.2.17/24, ")
	if err != nil || len(networks) != 3 {
		t.Fatalf("parse CIDRs: %v: %v", networks, err)
	}
	for _, tc := range []struct {
		ip   string
		want bool
	}{
		{"10.0.0.0", true}, {"10.255.255.255", true}, {"9.255.255.255", false}, {"11.0.0.0", false},
		{"::ffff:10.1.2.3", true}, {"2001:db8::1", true}, {"2001:db9::1", false},
		{"192.0.2.0", true}, {"192.0.2.255", true}, {"192.0.3.0", false}, {"invalid", false},
	} {
		t.Run(tc.ip, func(t *testing.T) {
			if got := containsAny(net.ParseIP(tc.ip), networks); got != tc.want {
				t.Fatalf("membership=%v, want %v", got, tc.want)
			}
		})
	}
	if _, err := parseCIDRList("10.0.0.0/8,invalid"); err == nil {
		t.Fatal("accepted partially invalid list")
	}
	if got, err := parseCIDRList(" "); err != nil || len(got) != 0 {
		t.Fatalf("empty list: %v, %v", got, err)
	}
}

func TestIPRangeFileFailures(t *testing.T) {
	setTestRanges(t)
	file := filepath.Join(t.TempDir(), "ranges.txt")
	if err := loadIPRanges(file); err == nil {
		t.Fatal("missing file accepted")
	}
	if !containsAny(net.ParseIP("10.1.2.3"), ipRanges) {
		t.Fatal("missing file cleared ranges")
	}
	if err := os.WriteFile(file, []byte(strings.Repeat("x", 70*1024)), 0600); err != nil {
		t.Fatal(err)
	}
	if err := loadIPRanges(file); err == nil {
		t.Fatal("oversized line accepted")
	}
	if !containsAny(net.ParseIP("10.1.2.3"), ipRanges) {
		t.Fatal("scanner failure cleared ranges")
	}
	if err := os.WriteFile(file, []byte("# deliberately empty\n\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := loadIPRanges(file); err != nil || len(ipRanges) != 0 {
		t.Fatalf("empty rules must route all clients externally: %v", err)
	}
}

func TestIPv6ProxyChain(t *testing.T) {
	trusted, _ := parseCIDRList("::1/128,2001:db8:ffff::/48")
	for _, tc := range []struct{ direct, xff, want string }{
		{"::1", "2001:db8::123,2001:db8:ffff::1", "2001:db8::123"},
		{"2001:db8::2", "2001:db8::123", "2001:db8::2"},
		{"::1", "2001:db8::123,garbage", "::1"},
	} {
		ctx := requestContext(tc.direct, "/")
		ctx.Request.Header.Set("X-Forwarded-For", tc.xff)
		if got := clientIP(ctx, trusted).String(); got != tc.want {
			t.Fatalf("%+v: got %s", tc, got)
		}
	}
}

func startHTTPGateway(t *testing.T, cfg *config) (string, *http.Client) {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &fasthttp.Server{Handler: makeHandler(cfg), ReadTimeout: cfg.RequestTimeout, WriteTimeout: cfg.RequestTimeout}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	transport := &http.Transport{DisableKeepAlives: true}
	client := &http.Client{Transport: transport, Timeout: 3 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	t.Cleanup(func() {
		transport.CloseIdleConnections()
		if err := server.Shutdown(); err != nil {
			t.Error(err)
		}
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("HTTP server did not stop")
		}
	})
	return "http://" + listener.Addr().String(), client
}

func TestHTTPGatewayBehavior(t *testing.T) {
	setTestRanges(t)
	logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := testConfig()
	cfg.trustedProxies, _ = parseCIDRList("127.0.0.1/32")
	gateway, client := startHTTPGateway(t, cfg)
	for _, tc := range []struct {
		name, method, uri, xff, xri, want string
		status                            int
	}{
		{"external by default", "GET", "/", "", "", cfg.ExternalRedirectURL, 302},
		{"campus forwarded", "GET", "/page?q=1&q=2", "10.1.2.3", "", cfg.InternalRedirectURL + "page?q=1&q=2", 302},
		{"real IP fallback", "GET", "/page", "", "10.1.2.3", cfg.InternalRedirectURL + "page", 302},
		{"spoofed leftmost IP", "GET", "/page", "10.1.2.3,198.51.100.8", "10.1.2.3", cfg.ExternalRedirectURL + "page", 302},
		{"malformed chain", "GET", "/page", "10.1.2.3,bad", "10.1.2.3", cfg.ExternalRedirectURL + "page", 302},
		{"existing prefix", "GET", "/jsxsd/page?q=%2F", "10.1.2.3", "", cfg.InternalRedirectURL + "page?q=%2F", 302},
		{"encoded path", "GET", "/a%2Fb/%E4%B8%AD%E6%96%87?q=hello+world", "", "", cfg.ExternalRedirectURL + "a%2Fb/%E4%B8%AD%E6%96%87?q=hello+world", 302},
		{"HEAD", "HEAD", "/page", "10.1.2.3", "", cfg.InternalRedirectURL + "page", 302},
		{"POST redirects", "POST", "/page", "10.1.2.3", "", cfg.InternalRedirectURL + "page", 302},
		{"encoded traversal", "GET", "/%2e%2e/admin", "", "", "", 400},
		{"encoded backslash", "GET", "/a%5Cb", "", "", "", 400},
		{"network path", "GET", "//evil.example/page", "", "", "", 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(tc.method, gateway+tc.uri, nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("X-Forwarded-For", tc.xff)
			req.Header.Set("X-Real-IP", tc.xri)
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tc.status || resp.Header.Get("Location") != tc.want {
				t.Fatalf("got %d, Location=%q; want %d, %q", resp.StatusCode, resp.Header.Get("Location"), tc.status, tc.want)
			}
			if tc.status == 302 && resp.Header.Get("Cache-Control") != "no-store" {
				t.Fatal("redirect may be cached across networks")
			}
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			if tc.method == "HEAD" && len(body) != 0 {
				t.Fatal("HEAD returned a body")
			}
		})
	}
}

func TestHealthRoutingAndPayload(t *testing.T) {
	setTestRanges(t)
	cfg := testConfig()
	cfg.HealthPath = "/ready"
	for _, tc := range []struct {
		path string
		want int
	}{{"/ready?detail=1", 200}, {"/health", 302}} {
		ctx := requestContext("203.0.113.1", tc.path)
		makeHandler(cfg)(ctx)
		if ctx.Response.StatusCode() != tc.want {
			t.Fatalf("%s: status %d", tc.path, ctx.Response.StatusCode())
		}
		if tc.want != 200 {
			continue
		}
		var response healthResponse
		if err := json.Unmarshal(ctx.Response.Body(), &response); err != nil {
			t.Fatal(err)
		}
		if response.Status != "ok" || response.IPRanges != 1 || response.Upstream != nil {
			t.Fatalf("wrong payload: %+v", response)
		}
		if _, err := time.Parse(time.RFC3339, response.Time); err != nil {
			t.Fatal(err)
		}
		if string(ctx.Response.Header.ContentType()) != "application/json" || string(ctx.Response.Header.Peek("Cache-Control")) != "no-store" {
			t.Fatal("wrong health headers")
		}
	}
	cfg.HealthPath = ""
	ctx := requestContext("203.0.113.1", "/health")
	makeHandler(cfg)(ctx)
	if ctx.Response.StatusCode() != 302 {
		t.Fatal("disabled health endpoint still intercepted request")
	}
}

func TestUpstreamStatusSemantics(t *testing.T) {
	for _, status := range []int{200, 204, 302, 399, 400, 403, 500, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" || r.URL.RequestURI() != "/entry?q=1" {
					t.Errorf("unexpected probe: %s %s", r.Method, r.URL)
				}
				w.Header().Set("Location", "/must-not-be-followed")
				w.WriteHeader(status)
			}))
			defer server.Close()
			want := "ok"
			if status >= 400 {
				want = fmt.Sprintf("status:%d", status)
			}
			if got := checkUpstream(server.URL+"/entry?q=1", time.Second); got != want {
				t.Fatalf("got %q, want %q", got, want)
			}
		})
	}
	t.Run("connection refused", func(t *testing.T) {
		server := httptest.NewServer(http.NotFoundHandler())
		target := server.URL
		server.Close()
		if got := checkUpstream(target, time.Second); !strings.HasPrefix(got, "unreachable: ") {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("oversized body", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, strings.Repeat("x", 1024*1024+1))
		}))
		defer server.Close()
		if got := checkUpstream(server.URL, time.Second); !strings.HasPrefix(got, "unreachable: ") {
			t.Fatalf("oversized body accepted: %q", got)
		}
	})
}

func TestHealthMixedUpstreams(t *testing.T) {
	setTestRanges(t)
	var internalCalls, externalCalls int32
	internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { atomic.AddInt32(&internalCalls, 1); w.WriteHeader(204) }))
	defer internal.Close()
	external := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { atomic.AddInt32(&externalCalls, 1); w.WriteHeader(503) }))
	defer external.Close()
	cfg := testConfig()
	cfg.CheckUpstream = true
	cfg.InternalRedirectURL, cfg.ExternalRedirectURL = internal.URL, external.URL
	ctx := requestContext("127.0.0.1", "/health")
	makeHandler(cfg)(ctx)
	var response healthResponse
	if err := json.Unmarshal(ctx.Response.Body(), &response); err != nil {
		t.Fatal(err)
	}
	if ctx.Response.StatusCode() != 503 || response.Status != "degraded" || response.Upstream["internal"] != "ok" || response.Upstream["external"] != "status:503" {
		t.Fatalf("unexpected mixed state: %d %+v", ctx.Response.StatusCode(), response)
	}
	if atomic.LoadInt32(&internalCalls) != 1 || atomic.LoadInt32(&externalCalls) != 1 {
		t.Fatal("both upstreams must be checked exactly once")
	}
}

func TestConcurrentReloadAndRequests(t *testing.T) {
	setTestRanges(t)
	logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	file := filepath.Join(t.TempDir(), "ranges.txt")
	if err := os.WriteFile(file, []byte("192.0.2.0/24\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := testConfig()
	handler := makeHandler(cfg)
	var workers sync.WaitGroup
	for i := 0; i < 6; i++ {
		workers.Add(1)
		go func(worker int) {
			defer workers.Done()
			for j := 0; j < 100; j++ {
				if worker == 0 {
					if err := loadIPRanges(file); err != nil {
						t.Error(err)
					}
					continue
				}
				ctx := requestContext("10.1.2.3", "/page")
				if j%2 == 0 {
					ctx.Request.SetRequestURI("/health")
				}
				handler(ctx)
				if got := ctx.Response.StatusCode(); got != 200 && got != 302 {
					t.Errorf("unexpected concurrent status: %d", got)
				}
			}
		}(i)
	}
	workers.Wait()
	if containsAny(net.ParseIP("10.1.2.3"), ipRanges) {
		t.Fatal("reload did not install final rules")
	}
}

func FuzzBuildTarget(f *testing.F) {
	for _, seed := range []string{"/", "/page?x=1&x=2", "/a%2Fb?q=%2F", "/jsxsd/page", "/%2e%2e/admin", "//evil.example", "/a%5Cb", "/?", ""} {
		f.Add(seed)
	}
	const base = "https://gateway.example/jsxsd/"
	f.Fuzz(func(t *testing.T, uri string) {
		target, err := buildTarget(base, uri)
		if err != nil {
			return
		}
		parsed, err := url.Parse(target)
		if err != nil {
			t.Fatalf("invalid output %q: %v", target, err)
		}
		if parsed.Scheme != "https" || parsed.Host != "gateway.example" || parsed.User != nil || parsed.Fragment != "" {
			t.Fatalf("destination escaped configured origin: %q", target)
		}
		if !strings.HasPrefix(parsed.EscapedPath(), "/jsxsd/") || unsafePath(parsed.Path) {
			t.Fatalf("unsafe output path: %q", target)
		}
		if uri == "" {
			uri = "/"
		}
		original, err := url.ParseRequestURI(uri)
		if err != nil {
			t.Fatalf("accepted invalid request: %q", uri)
		}
		if parsed.RawQuery != original.RawQuery || parsed.ForceQuery != original.ForceQuery {
			t.Fatalf("query changed: %q -> %q", uri, target)
		}
	})
}
