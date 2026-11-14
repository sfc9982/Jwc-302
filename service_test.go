package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestProbeCacheConcurrentAndExpiry(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		time.Sleep(30 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	var cache probeCache
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Go(func() {
			r := cache.get(time.Second, 150*time.Millisecond, srv.URL, srv.URL, nil)
			if r.internal != "ok" || r.external != "ok" {
				t.Errorf("probe: %+v", r)
			}
		})
	}
	wg.Wait()
	if n := calls.Load(); n != 2 {
		t.Fatalf("concurrent calls: %d", n)
	}
	cache.get(time.Second, 150*time.Millisecond, srv.URL, srv.URL, nil)
	if calls.Load() != 2 {
		t.Fatal("cache miss before expiry")
	}
	time.Sleep(180 * time.Millisecond)
	cache.get(time.Second, 150*time.Millisecond, srv.URL, srv.URL, nil)
	if calls.Load() != 4 {
		t.Fatal("cache did not expire")
	}
	cache.get(time.Second, 0, srv.URL, srv.URL, nil)
	if calls.Load() != 6 {
		t.Fatal("zero TTL did not refresh")
	}
}
func TestProbeCacheFailureAndRecovery(t *testing.T) {
	var failing atomic.Bool
	failing.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if failing.Load() {
			w.WriteHeader(503)
			return
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()
	var cache probeCache
	if r := cache.get(time.Second, time.Minute, srv.URL, srv.URL, nil); r.internal == "ok" {
		t.Fatal("expected failure")
	}
	failing.Store(false)
	if r := cache.get(time.Second, time.Minute, srv.URL, srv.URL, nil); r.internal == "ok" {
		t.Fatal("failure was not cached")
	}
	if r := cache.get(time.Second, 0, srv.URL, srv.URL, nil); r.internal != "ok" {
		t.Fatal("probe did not recover")
	}
}
func TestMetricsEndpointAndIsolation(t *testing.T) {
	cfg := testConfig()
	initLogger(cfg)
	ipRangesMu.Lock()
	ipRanges, excludedRanges = nil, nil
	ipRangesMu.Unlock()
	cfg.MetricsPath = "/metrics"
	first := makeHandler(cfg)
	second := makeHandler(cfg)
	first(requestContext("203.0.113.2", "/course"))
	ctx := requestContext("127.0.0.1", "/metrics")
	first(ctx)
	body := string(ctx.Response.Body())
	if ctx.Response.StatusCode() != 200 || !strings.Contains(body, `jwc302_redirects_total{code="302",route="external"} 1`) || !strings.Contains(body, "jwc302_rules_stale") {
		t.Fatalf("metrics response: %s", body)
	}
	other := requestContext("127.0.0.1", "/metrics")
	second(other)
	if strings.Contains(string(other.Response.Body()), "jwc302_redirects_total{") {
		t.Fatal("registry leaked across instances")
	}
	cfg.MetricsPath = cfg.ReadyPath
	if cfg.MetricsPath == "" {
		cfg.MetricsPath = cfg.HealthPath
	}
	if validateConfig(cfg) == nil {
		t.Fatal("expected path conflict")
	}
}
func TestVersionSkipsConfiguration(t *testing.T) {
	cfg, err := readConfig([]string{"-version", "-config=missing.json"}, func(string) string { return "" }, io.Discard)
	if err != nil || !cfg.Version {
		t.Fatalf("version: %+v %v", cfg, err)
	}
	if _, err = readConfig([]string{"-version", "-unknown"}, func(string) string { return "" }, io.Discard); err == nil {
		t.Fatal("invalid flag accepted")
	}
}

func TestProbeMetricsCountOnlyActualChecks(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(http.StatusOK) }))
	defer srv.Close()
	cfg := testConfig()
	cfg.ReadyPath = "/ready"
	cfg.MetricsPath = "/metrics"
	cfg.ReadyCheckUpstream = true
	cfg.UpstreamCacheTTL = time.Minute
	cfg.InternalRedirectURL = srv.URL
	cfg.ExternalRedirectURL = srv.URL
	handler := makeHandler(cfg)
	for i := 0; i < 3; i++ {
		ctx := requestContext("127.0.0.1", "/ready")
		handler(ctx)
		if ctx.Response.StatusCode() != 200 {
			t.Fatalf("ready status %d", ctx.Response.StatusCode())
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("actual probes: %d", calls.Load())
	}
	ctx := requestContext("127.0.0.1", "/metrics")
	handler(ctx)
	body := string(ctx.Response.Body())
	for _, want := range []string{`jwc302_upstream_probes_total{result="success",upstream="internal"} 1`, `jwc302_upstream_probes_total{result="success",upstream="external"} 1`, `jwc302_upstream_probe_duration_seconds_count{upstream="internal"} 1`} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %s", want)
		}
	}
}
func TestMetricsAndCacheConfiguration(t *testing.T) {
	cfg, err := readConfig([]string{"-metrics=/custom", "-upstream-cache-ttl=0"}, func(string) string { return "" }, io.Discard)
	if err != nil || cfg.MetricsPath != "/custom" || cfg.UpstreamCacheTTL != 0 {
		t.Fatalf("configuration: %+v %v", cfg, err)
	}
	for _, args := range [][]string{{"-metrics=/health"}, {"-metrics=/ready"}, {"-metrics=relative"}, {"-upstream-cache-ttl=-1s"}} {
		if _, err := readConfig(args, func(string) string { return "" }, io.Discard); err == nil {
			t.Errorf("accepted %v", args)
		}
	}
}

func TestReadinessReflectsStaleRulesWithCachedProbe(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(http.StatusOK) }))
	defer srv.Close()
	cfg := testConfig()
	cfg.ReadyPath = "/ready"
	cfg.ReadyCheckUpstream = true
	cfg.UpstreamCacheTTL = time.Minute
	cfg.InternalRedirectURL = srv.URL
	cfg.ExternalRedirectURL = srv.URL
	handler := makeHandler(cfg)
	ipRangesMu.Lock()
	oldFailed := ruleState.Failed
	ruleState.Failed = false
	ipRangesMu.Unlock()
	defer func() { ipRangesMu.Lock(); ruleState.Failed = oldFailed; ipRangesMu.Unlock() }()
	ready := requestContext("127.0.0.1", "/ready")
	handler(ready)
	if ready.Response.StatusCode() != 200 {
		t.Fatalf("initial status %d", ready.Response.StatusCode())
	}
	ipRangesMu.Lock()
	ruleState.Failed = true
	ipRangesMu.Unlock()
	ready = requestContext("127.0.0.1", "/ready")
	handler(ready)
	if ready.Response.StatusCode() != 503 {
		t.Fatalf("stale status %d", ready.Response.StatusCode())
	}
	if calls.Load() != 2 {
		t.Fatalf("unexpected probes %d", calls.Load())
	}
}

func TestReloadMetricsFailureAndRecovery(t *testing.T) {
	cfg := testConfig()
	initLogger(cfg)
	cfg.MetricsPath = "/metrics"
	cfg.ReloadInterval = 10 * time.Millisecond
	cfg.IPRangeFile = filepath.Join(t.TempDir(), "ranges.txt")
	m := newGatewayMetrics()
	ctx, cancel := context.WithCancel(context.Background())
	done := startReloader(ctx, cfg, m)
	defer func() { cancel(); <-done }()
	has := func(want string) bool {
		r := requestContext("127.0.0.1", "/metrics")
		m.handler(r)
		return strings.Contains(string(r.Response.Body()), want)
	}
	deadline := time.Now().Add(2 * time.Second)
	for !has(`jwc302_reloads_total{result="failure"}`) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !has(`jwc302_reloads_total{result="failure"}`) {
		t.Fatal("missing reload failure")
	}
	if err := os.WriteFile(cfg.IPRangeFile, []byte("10.0.0.0/8\n"), 0600); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(2 * time.Second)
	for !has(`jwc302_reloads_total{result="success"}`) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !has(`jwc302_reloads_total{result="success"}`) {
		t.Fatal("missing reload recovery")
	}
}
