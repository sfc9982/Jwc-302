//go:build integration

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

// This test builds and starts the real executable. No external university or VPN
// service is contacted: all assertions stop at the gateway's HTTP response.
func TestGatewayProcess(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "gateway")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	build := exec.Command("go", "build", "-o", binary, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build gateway: %v\n%s", err, output)
	}
	t.Run("startup rejects missing rules", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, binary, "-iprange="+filepath.Join(dir, "missing.txt"))
		cmd.Env = gatewayTestEnv()
		output, err := cmd.CombinedOutput()
		if ctx.Err() != nil || err == nil || !strings.Contains(string(output), "failed to load iprange file") {
			t.Fatalf("want immediate startup failure: %v\n%s", err, output)
		}
	})
	t.Run("check config without starting a listener", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "ranges.txt"), []byte("10.0.0.0/8\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "exclude.txt"), []byte("10.20.0.0/16\n"), 0600); err != nil {
			t.Fatal(err)
		}
		occupied, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer occupied.Close()
		file := filepath.Join(dir, "config.json")
		data, err := json.Marshal(map[string]any{"listen_addr": occupied.Addr().String(), "iprange_file": "ranges.txt", "exclude_iprange_file": "exclude.txt"})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, data, 0600); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, binary, "-config="+file, "-check-config")
		cmd.Env = gatewayTestEnv()
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("check-config: %v %s", err, output)
		}
		var result struct {
			Status   string `json:"status"`
			IPRanges int    `json:"ip_ranges"`
			Excluded int    `json:"excluded_ranges"`
		}
		if err := json.Unmarshal(output, &result); err != nil {
			t.Fatalf("check-config must emit JSON only and exit: %v %s", err, output)
		}
		if result.Status != "valid" || result.IPRanges != 1 || result.Excluded != 1 {
			t.Fatalf("unexpected validation result: %+v", result)
		}
		cmd = exec.CommandContext(ctx, binary, "-config="+file)
		cmd.Env = gatewayTestEnv()
		if output, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(output), "listen failed") {
			t.Fatalf("occupied listen address was not rejected: %v %s", err, output)
		}
		if err := os.WriteFile(filepath.Join(dir, "exclude.txt"), []byte("bad CIDR\n"), 0600); err != nil {
			t.Fatal(err)
		}
		cmd = exec.CommandContext(ctx, binary, "-config="+file, "-check-config")
		cmd.Env = gatewayTestEnv()
		if output, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(output), "failed to load iprange file") {
			t.Fatalf("check-config accepted invalid exclusion file: %v %s", err, output)
		}
	})
	for _, mode := range []struct {
		name string
		code int
	}{{"flags reload and lifecycle", 302}, {"JSON reload and lifecycle", 307}} {
		t.Run(mode.name, func(t *testing.T) {
			dir := t.TempDir()
			ranges := filepath.Join(dir, "ranges.txt")
			writeRules := func(content string) {
				t.Helper()
				// Rename a complete file so the ticker never sees a partially written CIDR.
				next := ranges + ".next"
				if err := os.WriteFile(next, []byte(content), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(next, ranges); err != nil {
					t.Fatal(err)
				}
			}
			writeRules("10.0.0.0/8\n")
			excludeFile := filepath.Join(dir, "exclude.txt")
			if err := os.WriteFile(excludeFile, []byte("10.20.0.0/16\n"), 0600); err != nil {
				t.Fatal(err)
			}
			listener, err := net.Listen("tcp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			addr := listener.Addr().String()
			if err := listener.Close(); err != nil {
				t.Fatal(err)
			}
			logPath := filepath.Join(dir, "gateway.log")
			logFile, err := os.Create(logPath)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = logFile.Close() })
			cmd := exec.Command(binary, "-addr="+addr, "-iprange="+ranges,
				"-internal=https://internal.example.edu/jsxsd/", "-external=https://vpn.example.edu/jsxsd/",
				"-trusted-proxies=127.0.0.1/32", "-reload-interval=50ms", "-request-timeout=1s", "-log-json")
			if mode.code == 307 {
				configFile := filepath.Join(dir, "config.json")
				content := `{"listen_addr":":9000","iprange_file":"ranges.txt","exclude_iprange_file":"exclude.txt","trusted_proxies":"127.0.0.1/32","reload_interval":"1m","request_timeout":"5s","log_json":true,"redirect_code":307}`
				if err := os.WriteFile(configFile, []byte(content), 0600); err != nil {
					t.Fatal(err)
				}
				cmd = exec.Command(binary, "-config="+configFile, "-addr="+addr, "-reload-interval=50ms", "-request-timeout=1s")
				cmd.Env = gatewayTestEnv("REQUEST_TIMEOUT=invalid-but-overridden")
			} else {
				cmd.Args = append(cmd.Args, "-exclude-iprange="+excludeFile)
				cmd.Env = gatewayTestEnv()
			}
			cmd.Stdout, cmd.Stderr = logFile, logFile
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			done := make(chan struct{})
			var waitErr error
			go func() { waitErr = cmd.Wait(); close(done) }()
			t.Cleanup(func() {
				select {
				case <-done:
				default:
					_ = cmd.Process.Kill()
					<-done
				}
				if t.Failed() {
					logs, _ := os.ReadFile(logPath)
					t.Logf("gateway logs:\n%s", logs)
				}
			})
			transport := &http.Transport{DisableKeepAlives: true}
			t.Cleanup(transport.CloseIdleConnections)
			client := &http.Client{Transport: transport, Timeout: time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
			base := "http://" + addr
			eventually := func(label string, check func() bool) {
				t.Helper()
				deadline := time.Now().Add(10 * time.Second)
				for time.Now().Before(deadline) {
					select {
					case <-done:
						t.Fatalf("gateway exited during %s: %v", label, waitErr)
					default:
					}
					if check() {
						return
					}
					time.Sleep(20 * time.Millisecond)
				}
				t.Fatalf("timed out waiting for %s", label)
			}
			eventually("health readiness", func() bool {
				resp, err := client.Get(base + "/health")
				if err != nil {
					return false
				}
				defer resp.Body.Close()
				var health healthResponse
				return json.NewDecoder(resp.Body).Decode(&health) == nil && resp.StatusCode == 200 && health.Status == "ok" && health.IPRanges == 1
			})
			redirect := func(xff, uri string) (int, string, string) {
				t.Helper()
				req, err := http.NewRequest("GET", base+uri, nil)
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set("X-Forwarded-For", xff)
				resp, err := client.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				defer resp.Body.Close()
				_, _ = io.Copy(io.Discard, resp.Body)
				return resp.StatusCode, resp.Header.Get("Location"), resp.Header.Get("Cache-Control")
			}
			assertRedirect := func(xff, want string) {
				t.Helper()
				status, target, cache := redirect(xff, "/jsxsd/course?q=%2F&x=1&x=2")
				if status != mode.code || target != want || cache != "no-store" {
					t.Fatalf("got %d, %q, cache=%q; want %q", status, target, cache, want)
				}
			}
			inside := "https://internal.example.edu/jsxsd/course?q=%2F&x=1&x=2"
			outside := "https://vpn.example.edu/jsxsd/course?q=%2F&x=1&x=2"
			assertRedirect("10.1.2.3", inside)
			assertRedirect("10.20.1.2", outside)
			assertRedirect("198.51.100.2", outside)
			assertRedirect("10.1.2.3,198.51.100.2", outside)
			if status, target, _ := redirect("10.1.2.3", "/%2e%2e/admin"); status != 400 || target != "" {
				t.Fatalf("unsafe path accepted: %d %q", status, target)
			}
			// A valid hot reload must change real requests without restarting the process.
			writeRules("198.51.100.0/24\n")
			eventually("valid CIDR reload", func() bool {
				status, target, _ := redirect("198.51.100.2", "/course")
				return status == mode.code && target == "https://internal.example.edu/jsxsd/course"
			})
			assertRedirect("10.1.2.3", outside)
			assertRedirect("198.51.100.2", inside)
			// Wait for an actual reload failure, then verify last-known-good routing.
			writeRules("invalid CIDR\n")
			eventually("failed reload log", func() bool {
				logs, _ := os.ReadFile(logPath)
				return strings.Contains(string(logs), "failed to reload iprange file")
			})
			assertRedirect("198.51.100.2", inside)
			assertRedirect("10.1.2.3", outside)
			for _, endpoint := range []struct {
				path   string
				status int
			}{{"/health", 200}, {"/ready", 503}} {
				resp, err := client.Get(base + endpoint.path)
				if err != nil {
					t.Fatal(err)
				}
				var health healthResponse
				err = json.NewDecoder(resp.Body).Decode(&health)
				resp.Body.Close()
				if err != nil || resp.StatusCode != endpoint.status || !health.RulesStale {
					t.Fatalf("stale rules %s: status %d %+v %v", endpoint.path, resp.StatusCode, health, err)
				}
			}
			writeRules("10.0.0.0/8\n2001:db8::/32\n")
			eventually("recovery after invalid rules", func() bool {
				resp, err := client.Get(base + "/health")
				if err != nil {
					return false
				}
				defer resp.Body.Close()
				var health healthResponse
				return json.NewDecoder(resp.Body).Decode(&health) == nil && health.IPRanges == 2 && !health.RulesStale
			})
			assertRedirect("10.1.2.3", inside)
			assertRedirect("2001:db8::123", inside)
			logs, err := os.ReadFile(logPath)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(logs), "q=%2F") {
				t.Fatal("default logs exposed query parameters")
			}
			foundRedirect := false
			for _, line := range strings.Split(strings.TrimSpace(string(logs)), "\n") {
				var entry map[string]interface{}
				if err := json.Unmarshal([]byte(line), &entry); err != nil {
					t.Fatalf("invalid JSON log: %v: %s", err, line)
				}
				if entry["msg"] == "redirect" && entry["status"] == float64(mode.code) && entry["real_ip"] == "10.1.2.3" {
					foundRedirect = true
				}
			}
			if !foundRedirect {
				t.Fatal("missing structured redirect log")
			}
			if runtime.GOOS != "windows" {
				if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
					t.Fatal(err)
				}
				select {
				case <-done:
					if waitErr != nil {
						t.Fatalf("graceful stop: %v", waitErr)
					}
				case <-time.After(12 * time.Second):
					t.Fatal("SIGTERM did not stop gateway")
				}
				logs, _ := os.ReadFile(logPath)
				if !strings.Contains(string(logs), "shutdown complete") {
					t.Fatal("missing graceful shutdown confirmation")
				}
				t.Log("SIGTERM graceful shutdown verified")
			} else {
				t.Log("Windows: SIGTERM check runs in Linux CI; process cleanup uses Kill")
			}
			t.Log(fmt.Sprintf("verified process on %s", runtime.GOOS))
		})
	}
}
