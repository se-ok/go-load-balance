package lib

import (
	"context"
	"log"
	"slices"
	"strconv"
	"strings"
	"time"
)

// StatusLogger logs periodic status information
type StatusLogger struct {
	pool     *Pool
	interval time.Duration
	verbose  bool
}

// NewStatusLogger creates a new status logger
func NewStatusLogger(pool *Pool, interval time.Duration, verbose bool) *StatusLogger {
	return &StatusLogger{
		pool:     pool,
		interval: interval,
		verbose:  verbose,
	}
}

// Start begins periodic status logging in a background goroutine
func (sl *StatusLogger) Start(ctx context.Context) {
	// Delay the first status log so the initial health check can complete.
	initialDelay := min(sl.interval, 5*time.Second)

	initialTimer := time.NewTimer(initialDelay)
	defer initialTimer.Stop()

	select {
	case <-ctx.Done():
		return
	case <-initialTimer.C:
		sl.logStatus()
	}

	ticker := time.NewTicker(sl.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sl.logStatus()
		}
	}
}

// connsSummary formats the active connection counts of healthy backends,
// sorted in decreasing order, e.g. "[60, 50, 45, 0]". With more than 30
// backends the middle is elided, keeping the first and last 15.
func connsSummary(backends []*Backend) string {
	conns := make([]int, 0, len(backends))
	for _, b := range backends {
		if b.IsHealthy() {
			conns = append(conns, b.GetActiveConns())
		}
	}
	slices.SortFunc(conns, func(a, b int) int { return b - a })

	if len(conns) <= 30 {
		return "[" + joinInts(conns) + "]"
	}
	return "[" + joinInts(conns[:15]) + ", ..., " + joinInts(conns[len(conns)-15:]) + "]"
}

func joinInts(nums []int) string {
	parts := make([]string, len(nums))
	for i, n := range nums {
		parts[i] = strconv.Itoa(n)
	}
	return strings.Join(parts, ", ")
}

// logStatus logs current status
func (sl *StatusLogger) logStatus() {
	totalActive, healthyCount, totalCount := sl.pool.GetStatus()

	// Always log summary
	log.Printf("[STATUS] Active: %d | Healthy: %d/%d | Conns/node: %s",
		totalActive, healthyCount, totalCount, connsSummary(sl.pool.GetBackends()))

	// Log per-backend breakdown if verbose
	if sl.verbose {
		backends := sl.pool.GetBackends()
		for _, backend := range backends {
			status := "unhealthy"
			if backend.IsHealthy() {
				status = "healthy"
			}
			activeConns := backend.GetActiveConns()
			log.Printf("[STATUS]   %s - %s, %d active", backend.URL.String(), status, activeConns)
		}
	}
}
