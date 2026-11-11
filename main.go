package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/valyala/fasthttp"
)

var (
	ipRanges   []*net.IPNet
	ipRangesMu sync.RWMutex

	logger *slog.Logger
)

type config struct {
	ListenAddr          string
	IPRangeFile         string
	InternalRedirectURL string
	ExternalRedirectURL string
	HealthPath          string
	ReloadInterval      time.Duration
	RequestTimeout      time.Duration
	CheckUpstream       bool
	LogJSON             bool
	trustedProxies      []*net.IPNet
}

func main() {
	cfg := parseConfig()
	initLogger(cfg)

	if err := loadIPRanges(cfg.IPRangeFile); err != nil {
		logger.Error("failed to load iprange file", "error", err)
		os.Exit(1)
	}
	ipRangesMu.RLock()
	logger.Info("loaded iprange entries", "file", cfg.IPRangeFile, "count", len(ipRanges))
	ipRangesMu.RUnlock()

	if cfg.ReloadInterval > 0 {
		startReloader(cfg.IPRangeFile, cfg.ReloadInterval)
	}

	server := &fasthttp.Server{
		Handler:      makeHandler(cfg),
		ReadTimeout:  cfg.RequestTimeout,
		WriteTimeout: cfg.RequestTimeout,
	}

	go func() {
		logger.Info("server started", "addr", cfg.ListenAddr)
		if err := server.ListenAndServe(cfg.ListenAddr); err != nil {
			logger.Error("server error", "error", err)
			os.Exit(1)
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	logger.Info("shutting down gracefully")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := server.Shutdown(); err != nil {
			logger.Error("shutdown error", "error", err)
		}
	}()

	select {
	case <-done:
		logger.Info("shutdown complete")
	case <-shutdownCtx.Done():
		logger.Warn("shutdown timed out")
	}
}

func parseConfig() *config {
	listenAddr := flag.String("addr", envOrDefault("LISTEN_ADDR", "127.0.0.1:8080"), "listen address")
	ipRangeFile := flag.String("iprange", envOrDefault("IPRANGE_FILE", "iprange.txt"), "path to iprange file")
	internalURL := flag.String("internal", envOrDefault("INTERNAL_URL", "https://internal.example.edu/jsxsd/"), "internal redirect base URL")
	externalURL := flag.String("external", envOrDefault("EXTERNAL_URL", "https://vpn.example.edu/jsxsd/"), "external redirect base URL")
	healthPath := flag.String("health", envOrDefault("HEALTH_PATH", "/health"), "health check path")
	trustedProxiesStr := flag.String("trusted-proxies", envOrDefault("TRUSTED_PROXIES", ""), "trusted proxy CIDRs, comma separated")
	reloadInterval := flag.Duration("reload-interval", parseDurationEnv("RELOAD_INTERVAL", 5*time.Minute), "iprange reload interval (0 to disable)")
	requestTimeout := flag.Duration("request-timeout", parseDurationEnv("REQUEST_TIMEOUT", 5*time.Second), "request timeout")
	checkUpstream := flag.Bool("check-upstream", parseBoolEnv("CHECK_UPSTREAM", false), "check upstream URLs in health endpoint")
	logJSON := flag.Bool("log-json", parseBoolEnv("LOG_JSON", false), "output JSON logs")
	flag.Parse()

	trusted, err := parseCIDRList(*trustedProxiesStr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid trusted-proxies: %v\n", err)
		os.Exit(1)
	}

	cfg := &config{
		ListenAddr:          *listenAddr,
		IPRangeFile:         *ipRangeFile,
		InternalRedirectURL: normalizeBaseURL(*internalURL),
		ExternalRedirectURL: normalizeBaseURL(*externalURL),
		HealthPath:          *healthPath,
		ReloadInterval:      *reloadInterval,
		RequestTimeout:      *requestTimeout,
		CheckUpstream:       *checkUpstream,
		LogJSON:             *logJSON,
		trustedProxies:      trusted,
	}
	if err := validateConfig(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "invalid configuration: %v\n", err)
		os.Exit(1)
	}
	return cfg
}

func validateConfig(cfg *config) error {
	if cfg.RequestTimeout <= 0 || cfg.ReloadInterval < 0 {
		return fmt.Errorf("request-timeout must be positive and reload-interval must be non-negative")
	}
	if _, _, err := net.SplitHostPort(cfg.ListenAddr); err != nil {
		return fmt.Errorf("invalid listen address: %w", err)
	}
	if cfg.HealthPath != "" && (!strings.HasPrefix(cfg.HealthPath, "/") || strings.ContainsAny(cfg.HealthPath, "?#")) {
		return fmt.Errorf("health path must start with / and contain no query or fragment")
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

func initLogger(cfg *config) {
	opts := &slog.HandlerOptions{Level: slog.LevelInfo}
	var handler slog.Handler
	if cfg.LogJSON {
		handler = slog.NewJSONHandler(os.Stdout, opts)
	} else {
		handler = slog.NewTextHandler(os.Stdout, opts)
	}
	logger = slog.New(handler)
}

func normalizeBaseURL(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return s
	}
	if !strings.HasSuffix(s, "/") {
		s += "/"
	}
	return s
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func parseDurationEnv(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid %s: %v\n", key, err)
		os.Exit(1)
	}
	return d
}

func parseBoolEnv(key string, def bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	switch strings.ToLower(v) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		fmt.Fprintf(os.Stderr, "invalid %s: expected bool\n", key)
		os.Exit(1)
		return def
	}
}

func parseCIDRList(s string) ([]*net.IPNet, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	var out []*net.IPNet
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		_, ipNet, err := net.ParseCIDR(part)
		if err != nil {
			return nil, fmt.Errorf("invalid CIDR %q: %w", part, err)
		}
		out = append(out, ipNet)
	}
	return out, nil
}

func loadIPRanges(filename string) error {
	file, err := os.Open(filename)
	if err != nil {
		return fmt.Errorf("open %s: %w", filename, err)
	}
	defer file.Close()

	var ranges []*net.IPNet
	scanner := bufio.NewScanner(file)
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		_, ipNet, err := net.ParseCIDR(line)
		if err != nil {
			return fmt.Errorf("parse line %d %q: %w", lineNum, line, err)
		}
		ranges = append(ranges, ipNet)
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read %s: %w", filename, err)
	}

	ipRangesMu.Lock()
	ipRanges = ranges
	ipRangesMu.Unlock()
	return nil
}

func startReloader(filename string, interval time.Duration) {
	ticker := time.NewTicker(interval)
	go func() {
		for range ticker.C {
			if err := loadIPRanges(filename); err != nil {
				logger.Warn("failed to reload iprange file", "error", err)
				continue
			}
			ipRangesMu.RLock()
			count := len(ipRanges)
			ipRangesMu.RUnlock()
			logger.Info("reloaded iprange file", "file", filename, "count", count)
		}
	}()
}

func makeHandler(cfg *config) func(*fasthttp.RequestCtx) {
	return func(ctx *fasthttp.RequestCtx) {
		path := string(ctx.Path())
		if cfg.HealthPath != "" && path == cfg.HealthPath {
			healthHandler(ctx, cfg)
			return
		}

		realIP := clientIP(ctx, cfg.trustedProxies)

		ipRangesMu.RLock()
		ranges := make([]*net.IPNet, len(ipRanges))
		copy(ranges, ipRanges)
		ipRangesMu.RUnlock()

		matched := false
		for _, ipNet := range ranges {
			if ipNet.Contains(realIP) {
				matched = true
				break
			}
		}

		base := cfg.ExternalRedirectURL
		if matched {
			base = cfg.InternalRedirectURL
		}
		target, err := buildTarget(base, string(ctx.RequestURI()))
		if err != nil {
			ctx.Error("invalid request path", fasthttp.StatusBadRequest)
			return
		}

		ctx.Response.Header.Set("Cache-Control", "no-store")
		ctx.Response.Header.Set("Location", target)
		ctx.SetStatusCode(fasthttp.StatusFound)

		logger.Info("redirect",
			"method", string(ctx.Method()),
			"uri", string(ctx.RequestURI()),
			"direct_ip", ctx.RemoteIP(),
			"real_ip", realIP,
			"matched", matched,
			"target", target,
			"status", fasthttp.StatusFound,
		)
	}
}

func clientIP(ctx *fasthttp.RequestCtx, trusted []*net.IPNet) net.IP {
	direct := ctx.RemoteIP()
	if direct == nil || !containsAny(direct, trusted) {
		return direct
	}

	if xff := ctx.Request.Header.Peek("X-Forwarded-For"); len(xff) > 0 {
		parts := strings.Split(string(xff), ",")
		// Take the rightmost non-trusted IP as the real client.
		for i := len(parts) - 1; i >= 0; i-- {
			ip := net.ParseIP(strings.TrimSpace(parts[i]))
			if ip == nil {
				return direct
			}
			if !containsAny(ip, trusted) {
				return ip
			}
		}
		// All proxies in the chain are trusted; return the original client.
		if ip := net.ParseIP(strings.TrimSpace(parts[0])); ip != nil {
			return ip
		}
		return direct
	}
	if xri := ctx.Request.Header.Peek("X-Real-Ip"); len(xri) > 0 {
		if ip := net.ParseIP(strings.TrimSpace(string(xri))); ip != nil {
			return ip
		}
	}

	return direct
}

func containsAny(ip net.IP, networks []*net.IPNet) bool {
	for _, ipNet := range networks {
		if ipNet.Contains(ip) {
			return true
		}
	}
	return false
}

func unsafePath(path string) bool {
	if strings.Contains(path, "\\") {
		return true
	}
	for _, segment := range strings.Split(path, "/") {
		if segment == "." || segment == ".." {
			return true
		}
	}
	return false
}

func buildTarget(base, requestURI string) (string, error) {
	u, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	if requestURI == "" {
		requestURI = "/"
	}
	if !strings.HasPrefix(requestURI, "/") || strings.HasPrefix(requestURI, "//") || strings.Contains(requestURI, "#") {
		return "", fmt.Errorf("expected an origin-form request URI")
	}
	rel, err := url.ParseRequestURI(requestURI)
	if err != nil {
		return "", err
	}
	if unsafePath(rel.Path) {
		return "", fmt.Errorf("path traversal is not allowed")
	}
	basePath := u.EscapedPath()
	requestPath := rel.EscapedPath()
	// Accept both /page and /jsxsd/page without duplicating the base.
	if requestPath == strings.TrimSuffix(basePath, "/") {
		requestPath = ""
	} else if strings.HasPrefix(requestPath, basePath) {
		requestPath = strings.TrimPrefix(requestPath, basePath)
	}
	u.RawPath = basePath + strings.TrimLeft(requestPath, "/")
	u.Path, err = url.PathUnescape(u.RawPath)
	if err != nil {
		return "", err
	}
	u.RawQuery, u.ForceQuery = rel.RawQuery, rel.ForceQuery
	return u.String(), nil
}

type healthResponse struct {
	Status   string            `json:"status"`
	Time     string            `json:"time"`
	IPRanges int               `json:"ip_ranges"`
	Upstream map[string]string `json:"upstream,omitempty"`
}

func healthHandler(ctx *fasthttp.RequestCtx, cfg *config) {
	resp := healthResponse{
		Status: "ok",
		Time:   time.Now().Format(time.RFC3339),
	}
	ipRangesMu.RLock()
	resp.IPRanges = len(ipRanges)
	ipRangesMu.RUnlock()

	if cfg.CheckUpstream {
		// Reserve half the response timeout for scheduling and writing the result.
		timeout := cfg.RequestTimeout - cfg.RequestTimeout/2
		var internal, external string
		var checks sync.WaitGroup
		checks.Add(2)
		go func() {
			defer checks.Done()
			internal = checkUpstream(cfg.InternalRedirectURL, timeout)
		}()
		go func() {
			defer checks.Done()
			external = checkUpstream(cfg.ExternalRedirectURL, timeout)
		}()
		checks.Wait()
		resp.Upstream = map[string]string{"internal": internal, "external": external}
		if internal != "ok" || external != "ok" {
			resp.Status = "degraded"
		}
	}

	data, err := json.Marshal(resp)
	if err != nil {
		ctx.Error(err.Error(), fasthttp.StatusInternalServerError)
		return
	}
	ctx.SetContentType("application/json")
	ctx.SetStatusCode(fasthttp.StatusOK)
	if resp.Status != "ok" {
		ctx.SetStatusCode(fasthttp.StatusServiceUnavailable)
	}
	ctx.Response.Header.Set("Cache-Control", "no-store")
	ctx.Write(data)
}

var upstreamClient = &fasthttp.Client{MaxResponseBodySize: 1024 * 1024}

func checkUpstream(target string, timeout time.Duration) string {
	req := fasthttp.AcquireRequest()
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(resp)

	req.SetRequestURI(target)
	req.Header.SetMethod(fasthttp.MethodGet)

	if err := upstreamClient.DoTimeout(req, resp, timeout); err != nil {
		return "unreachable: " + err.Error()
	}
	if resp.StatusCode() >= 200 && resp.StatusCode() < 400 {
		return "ok"
	}
	return fmt.Sprintf("status:%d", resp.StatusCode())
}
