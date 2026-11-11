package main

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"time"
)

var (
	ipRanges       []*net.IPNet
	excludedRanges []*net.IPNet
	ipRangesMu     sync.RWMutex
	ruleState      struct {
		LastSuccess time.Time
		Failed      bool
	}
)

func parseCIDRList(s string) ([]*net.IPNet, error) {
	var out []*net.IPNet
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		_, network, err := net.ParseCIDR(part)
		if err != nil {
			return nil, fmt.Errorf("invalid CIDR %q: %w", part, err)
		}
		out = append(out, network)
	}
	return out, nil
}

func readIPRanges(filename string) ([]*net.IPNet, error) {
	file, err := os.Open(filename)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", filename, err)
	}
	defer file.Close()
	var ranges []*net.IPNet
	scanner := bufio.NewScanner(file)
	for lineNum := 1; scanner.Scan(); lineNum++ {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		_, network, err := net.ParseCIDR(line)
		if err != nil {
			return nil, fmt.Errorf("parse line %d %q: %w", lineNum, line, err)
		}
		ranges = append(ranges, network)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read %s: %w", filename, err)
	}
	return ranges, nil
}

// Both files form one routing snapshot. An error in either preserves both old
// lists, while readiness reports the failed reload until a successful retry.
func loadRangeFiles(includeFile, excludeFile string) error {
	include, err := readIPRanges(includeFile)
	var exclude []*net.IPNet
	if err == nil && excludeFile != "" {
		exclude, err = readIPRanges(excludeFile)
	}
	ipRangesMu.Lock()
	defer ipRangesMu.Unlock()
	if err != nil {
		ruleState.Failed = true
		return err
	}
	ipRanges, excludedRanges = include, exclude
	ruleState.LastSuccess = time.Now()
	ruleState.Failed = false
	return nil
}

func loadIPRanges(filename string) error { return loadRangeFiles(filename, "") }

func startReloader(ctx context.Context, cfg *config) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(cfg.ReloadInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := loadRangeFiles(cfg.IPRangeFile, cfg.ExcludeIPRangeFile); err != nil {
					logger.Warn("failed to reload iprange file", "error", err)
					continue
				}
				logger.Info("reloaded iprange files")
			}
		}
	}()
	return done
}
