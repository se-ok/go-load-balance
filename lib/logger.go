package lib

import (
	"context"
	"log"
	"time"
)

// StatusLogger logs periodic status information
type StatusLogger struct {
	pool    *Pool
	verbose bool
}

// NewStatusLogger creates a new status logger
func NewStatusLogger(pool *Pool, verbose bool) *StatusLogger {
	return &StatusLogger{
		pool:    pool,
		verbose: verbose,
	}
}

// Start begins periodic status logging in a background goroutine
func (sl *StatusLogger) Start(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	// Log initial status immediately
	sl.logStatus()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sl.logStatus()
		}
	}
}

// logStatus logs current status
func (sl *StatusLogger) logStatus() {
	totalActive, healthyCount, totalCount := sl.pool.GetStatus()

	// Always log summary
	log.Printf("[STATUS] Active: %d | Healthy: %d/%d", totalActive, healthyCount, totalCount)

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
