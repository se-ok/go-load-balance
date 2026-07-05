package lib

import (
	"context"
	"log"
	"net/http"
	"sync"
	"time"
)

// HealthChecker performs periodic health checks on backends
type HealthChecker struct {
	pool     *Pool
	interval time.Duration
	client   *http.Client
}

// NewHealthChecker creates a new health checker
func NewHealthChecker(pool *Pool, interval time.Duration) *HealthChecker {
	return &HealthChecker{
		pool:     pool,
		interval: interval,
		client: &http.Client{
			Timeout: 5 * time.Second, // Short timeout for health checks
		},
	}
}

// Start begins periodic health checking in a background goroutine
func (hc *HealthChecker) Start(ctx context.Context) {
	ticker := time.NewTicker(hc.interval)
	defer ticker.Stop()

	// Run initial health check immediately
	hc.checkAll()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			hc.checkAll()
		}
	}
}

// checkAll checks health of all backends concurrently, so a handful of
// timing-out backends cannot make the sweep overrun the check interval.
func (hc *HealthChecker) checkAll() {
	backends := hc.pool.GetBackends()
	var wg sync.WaitGroup
	for _, backend := range backends {
		wg.Go(func() { hc.checkBackend(backend) })
	}
	wg.Wait()
}

// checkBackend checks health of a single backend
func (hc *HealthChecker) checkBackend(backend *Backend) {
	// Health check endpoint: /v1/models
	healthURL := backend.URL.String() + "/v1/models"

	resp, err := hc.client.Get(healthURL)
	if err != nil {
		// Connection error
		if backend.MarkUnhealthy() {
			log.Printf("[HEALTH] %s marked as unhealthy (error: %v)", backend.URL.String(), err)
		}
		return
	}
	defer resp.Body.Close()

	// Check if response is 2xx
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if backend.RecordCheckSuccess() {
			log.Printf("[HEALTH] %s marked as healthy", backend.URL.String())
		}
	} else {
		if backend.MarkUnhealthy() {
			log.Printf("[HEALTH] %s marked as unhealthy (status: %d)", backend.URL.String(), resp.StatusCode)
		}
	}
}
