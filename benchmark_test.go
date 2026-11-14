package main

import (
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"
)

func BenchmarkCIDR(b *testing.B) {
	for _, family := range []string{"ipv4", "ipv6"} {
		for _, n := range []int{10, 1000, 10000} {
			for _, pos := range []string{"first", "last", "miss", "excluded"} {
				b.Run(fmt.Sprintf("%s/%d/%s", family, n, pos), func(b *testing.B) {
					networks := make([]*net.IPNet, n)
					for i := range networks {
						var cidr string
						if family == "ipv4" {
							cidr = fmt.Sprintf("10.%d.%d.0/24", i/256, i%256)
						} else {
							cidr = fmt.Sprintf("2001:db8:%x::/48", i)
						}
						_, networks[i], _ = net.ParseCIDR(cidr)
					}
					index := 0
					if pos == "last" || pos == "excluded" {
						index = n - 1
					}
					ip := networks[index].IP
					if pos == "miss" {
						if family == "ipv4" {
							ip = net.ParseIP("192.0.2.1")
						} else {
							ip = net.ParseIP("2001:db9::1")
						}
					}
					b.ReportAllocs()
					b.ResetTimer()
					for i := 0; i < b.N; i++ {
						_ = containsAny(ip, networks)
					}
				})
			}
		}
	}
}
func BenchmarkProxyChain(b *testing.B) {
	_, trusted, _ := net.ParseCIDR("127.0.0.0/8")
	for _, hops := range []int{1, 10, 100} {
		b.Run(fmt.Sprint(hops), func(b *testing.B) {
			ctx := requestContext("127.0.0.1", "/")
			ctx.Request.Header.Set("X-Forwarded-For", "192.0.2.1"+strings.Repeat(", 127.0.0.1", hops-1))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_ = clientIP(ctx, []*net.IPNet{trusted})
			}
		})
	}
}
func BenchmarkBuildTarget(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _ = buildTarget("https://example.org/jsxsd/", "/course?id=1&id=2")
	}
}
func BenchmarkRedirect(b *testing.B) {
	logger = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	for _, enabled := range []bool{false, true} {
		for _, parallel := range []bool{false, true} {
			b.Run(fmt.Sprintf("metrics=%t/parallel=%t", enabled, parallel), func(b *testing.B) {
				cfg := testConfig()
				if enabled {
					cfg.MetricsPath = "/metrics"
				}
				handler := makeHandler(cfg)
				b.ReportAllocs()
				b.ResetTimer()
				if parallel {
					b.RunParallel(func(pb *testing.PB) {
						ctx := requestContext("192.0.2.1", "/course?id=1")
						for pb.Next() {
							handler(ctx)
						}
					})
				} else {
					ctx := requestContext("192.0.2.1", "/course?id=1")
					for i := 0; i < b.N; i++ {
						handler(ctx)
					}
				}
			})
		}
	}
}
func BenchmarkProbeCacheHit(b *testing.B) {
	p := probeCache{result: probeResult{internal: "ok", external: "ok", checkedAt: time.Now()}}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = p.get(time.Second, time.Hour, "", "", nil)
	}
}
func BenchmarkLogEncoding(b *testing.B) {
	for _, json := range []bool{false, true} {
		b.Run(fmt.Sprintf("json=%t", json), func(b *testing.B) {
			var h slog.Handler
			if json {
				h = slog.NewJSONHandler(io.Discard, nil)
			} else {
				h = slog.NewTextHandler(io.Discard, nil)
			}
			l := slog.New(h)
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				l.Info("redirect", "method", "GET", "uri", "/course", "matched", true, "status", 302)
			}
		})
	}
}
