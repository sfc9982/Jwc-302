package main

import (
	"context"
	"encoding/json"
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

var logger *slog.Logger

func main() { os.Exit(run()) }

func run() int {
	cfg := parseConfig()
	initLogger(cfg)
	if err := loadRangeFiles(cfg.IPRangeFile, cfg.ExcludeIPRangeFile); err != nil {
		logger.Error("failed to load iprange file", "error", err)
		return 1
	}
	ipRangesMu.RLock()
	included, excluded := len(ipRanges), len(excludedRanges)
	ipRangesMu.RUnlock()
	if cfg.CheckConfig {
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"status": "valid", "ip_ranges": included, "excluded_ranges": excluded})
		return 0
	}
	listener, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		logger.Error("listen failed", "error", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if cfg.ReloadInterval > 0 {
		startReloader(ctx, cfg)
	}
	server := &fasthttp.Server{Handler: makeHandler(cfg), ReadTimeout: cfg.RequestTimeout, WriteTimeout: cfg.RequestTimeout}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	logger.Info("server started", "addr", listener.Addr().String(), "ip_ranges", included, "excluded_ranges", excluded)
	select {
	case err := <-done:
		if err != nil {
			logger.Error("server error", "error", err)
			return 1
		}
		return 0
	case <-ctx.Done():
	}
	stop()
	logger.Info("shutting down gracefully")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	if err := server.ShutdownWithContext(shutdownCtx); err != nil {
		logger.Error("shutdown timed out", "error", err)
		return 1
	}
	logger.Info("shutdown complete")
	return 0
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

func makeHandler(cfg *config) func(*fasthttp.RequestCtx) {
	return func(ctx *fasthttp.RequestCtx) {
		request, err := parseOriginURI(string(ctx.RequestURI()))
		if err != nil {
			ctx.Error("invalid request path", fasthttp.StatusBadRequest)
			return
		}
		path := request.Path
		if cfg.HealthPath != "" && path == cfg.HealthPath {
			healthHandler(ctx, cfg)
			return
		}

		if cfg.ReadyPath != "" && path == cfg.ReadyPath {
			serveHealth(ctx, cfg, true)
			return
		}
		realIP := clientIP(ctx, cfg.trustedProxies)

		ipRangesMu.RLock()
		excluded := containsAny(realIP, excludedRanges)
		matched := containsAny(realIP, ipRanges) && !excluded
		ipRangesMu.RUnlock()

		base := cfg.ExternalRedirectURL
		if matched {
			base = cfg.InternalRedirectURL
		}
		target, err := buildTargetFromURI(base, request)
		if err != nil {
			ctx.Error("invalid request path", fasthttp.StatusBadRequest)
			return
		}

		ctx.Response.Header.Set("Cache-Control", "no-store")
		ctx.Response.Header.Set("Location", target)
		ctx.SetStatusCode(cfg.RedirectCode)

		logURI, logTarget := string(ctx.RequestURI()), target
		if !cfg.LogQuery {
			logURI, _, _ = strings.Cut(logURI, "?")
			logTarget, _, _ = strings.Cut(logTarget, "?")
		}
		logger.Info("redirect",
			"method", string(ctx.Method()),
			"uri", logURI,
			"direct_ip", ctx.RemoteIP(),
			"real_ip", realIP,
			"matched", matched,
			"target", logTarget,
			"excluded", excluded,
			"status", cfg.RedirectCode,
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

func parseOriginURI(requestURI string) (*url.URL, error) {
	if requestURI == "" {
		requestURI = "/"
	}
	if !strings.HasPrefix(requestURI, "/") || strings.HasPrefix(requestURI, "//") || strings.Contains(requestURI, "#") {
		return nil, fmt.Errorf("expected an origin-form request URI")
	}
	rel, err := url.ParseRequestURI(requestURI)
	if err != nil {
		return nil, err
	}
	if unsafePath(rel.Path) {
		return nil, fmt.Errorf("path traversal is not allowed")
	}
	return rel, nil
}

func buildTarget(base, requestURI string) (string, error) {
	request, err := parseOriginURI(requestURI)
	if err != nil {
		return "", err
	}
	return buildTargetFromURI(base, request)
}

func buildTargetFromURI(base string, rel *url.URL) (string, error) {
	u, err := url.Parse(base)
	if err != nil {
		return "", err
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
	Status         string            `json:"status"`
	Time           string            `json:"time"`
	IPRanges       int               `json:"ip_ranges"`
	Upstream       map[string]string `json:"upstream,omitempty"`
	ExcludedRanges int               `json:"excluded_ranges"`
	LastReload     string            `json:"last_reload,omitempty"`
	RulesStale     bool              `json:"rules_stale"`
}

func healthHandler(ctx *fasthttp.RequestCtx, cfg *config) { serveHealth(ctx, cfg, false) }

func serveHealth(ctx *fasthttp.RequestCtx, cfg *config, readiness bool) {
	resp := healthResponse{
		Status: "ok",
		Time:   time.Now().Format(time.RFC3339),
	}
	ipRangesMu.RLock()
	resp.IPRanges = len(ipRanges)
	resp.ExcludedRanges = len(excludedRanges)
	resp.RulesStale = ruleState.Failed
	if !ruleState.LastSuccess.IsZero() {
		resp.LastReload = ruleState.LastSuccess.Format(time.RFC3339)
	}
	ipRangesMu.RUnlock()
	if readiness && resp.RulesStale {
		resp.Status = "degraded"
	}

	if cfg.CheckUpstream || (readiness && cfg.ReadyCheckUpstream) {
		// Reserve half the response timeout for scheduling and writing the result.
		timeout := cfg.RequestTimeout - cfg.RequestTimeout/2
		var internal, external string
		var checks sync.WaitGroup
		checks.Go(func() { internal = checkUpstream(cfg.InternalRedirectURL, timeout) })
		checks.Go(func() { external = checkUpstream(cfg.ExternalRedirectURL, timeout) })
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
