package main

import (
	"runtime"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/valyala/fasthttp"
	"github.com/valyala/fasthttp/fasthttpadaptor"
)

type service struct {
	cfg     *config
	probes  probeCache
	metrics *gatewayMetrics
}

func newService(cfg *config) *service {
	s := &service{cfg: cfg}
	if cfg.MetricsPath != "" {
		s.metrics = newGatewayMetrics()
	}
	return s
}

type probeResult struct {
	internal, external string
	checkedAt          time.Time
}
type probeFlight struct {
	done   chan struct{}
	result probeResult
}
type probeCache struct {
	mu     sync.Mutex
	result probeResult
	flight *probeFlight
}

func (p *probeCache) get(budget, ttl time.Duration, internal, external string, metrics *gatewayMetrics) probeResult {
	p.mu.Lock()
	if ttl > 0 && !p.result.checkedAt.IsZero() && time.Since(p.result.checkedAt) < ttl {
		r := p.result
		p.mu.Unlock()
		return r
	}
	f := p.flight
	if f == nil {
		f = &probeFlight{done: make(chan struct{})}
		p.flight = f
		go func() {
			timeout := budget - budget/2
			var r probeResult
			var wg sync.WaitGroup
			wg.Go(func() { r.internal = measuredProbe("internal", internal, timeout, metrics) })
			wg.Go(func() { r.external = measuredProbe("external", external, timeout, metrics) })
			wg.Wait()
			r.checkedAt = time.Now()
			p.mu.Lock()
			f.result = r
			p.result = r
			p.flight = nil
			close(f.done)
			p.mu.Unlock()
		}()
	}
	p.mu.Unlock()
	timer := time.NewTimer(budget - budget/4)
	defer timer.Stop()
	select {
	case <-f.done:
		return f.result
	case <-timer.C:
		return probeResult{internal: "unreachable: probe timed out", external: "unreachable: probe timed out"}
	}
}
func measuredProbe(name, target string, timeout time.Duration, m *gatewayMetrics) string {
	start := time.Now()
	result := checkUpstream(target, timeout)
	if m != nil {
		status := "failure"
		if result == "ok" {
			status = "success"
		}
		m.probes.WithLabelValues(name, status).Inc()
		m.duration.WithLabelValues(name).Observe(time.Since(start).Seconds())
	}
	return result
}

type gatewayMetrics struct {
	redirects *prometheus.CounterVec
	reloads   *prometheus.CounterVec
	probes    *prometheus.CounterVec
	duration  *prometheus.HistogramVec
	handler   fasthttp.RequestHandler
}

func newGatewayMetrics() *gatewayMetrics {
	m := &gatewayMetrics{
		redirects: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "jwc302_redirects_total", Help: "Completed redirects by route and HTTP status."}, []string{"route", "code"}),
		reloads:   prometheus.NewCounterVec(prometheus.CounterOpts{Name: "jwc302_reloads_total", Help: "Scheduled rule reloads by result."}, []string{"result"}),
		probes:    prometheus.NewCounterVec(prometheus.CounterOpts{Name: "jwc302_upstream_probes_total", Help: "Actual upstream probes by result."}, []string{"upstream", "result"}),
		duration:  prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "jwc302_upstream_probe_duration_seconds", Help: "Duration of actual upstream probes."}, []string{"upstream"}),
	}
	registry := prometheus.NewRegistry()
	registry.MustRegister(m.redirects, m.reloads, m.probes, m.duration)
	snapshot := func(which string) float64 {
		ipRangesMu.RLock()
		defer ipRangesMu.RUnlock()
		switch which {
		case "stale":
			if ruleState.Failed {
				return 1
			}
		case "include":
			return float64(len(ipRanges))
		case "exclude":
			return float64(len(excludedRanges))
		case "last":
			if !ruleState.LastSuccess.IsZero() {
				return float64(ruleState.LastSuccess.Unix())
			}
		}
		return 0
	}
	registry.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "jwc302_rules_stale", Help: "Whether the latest rule reload failed."}, func() float64 { return snapshot("stale") }))
	for _, kind := range []string{"include", "exclude"} {
		k := kind
		registry.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "jwc302_rules_count", Help: "Number of loaded rules.", ConstLabels: prometheus.Labels{"kind": k}}, func() float64 { return snapshot(k) }))
	}
	registry.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "jwc302_last_reload_success_timestamp_seconds", Help: "Unix time of last successful rule load."}, func() float64 { return snapshot("last") }))
	build := prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "jwc302_build_info", Help: "Build information."}, []string{"version", "commit", "go_version"})
	build.WithLabelValues(version, commit, runtime.Version()).Set(1)
	registry.MustRegister(build)
	m.handler = fasthttpadaptor.NewFastHTTPHandler(promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))
	return m
}
