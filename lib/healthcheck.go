package lib

import (
	"context"
	"log"
	"net/http"
	"time"
)

// HealthChecker performs periodic health checks on backends
type HealthChecker struct {
	pool     *Pool
	interval time.Duration
	client   *http.Client
}

// NewHealthChecker creates a new health checker
func NewHealthChecker(pool *Pool, interval time.Duration, timeout time.Duration) *HealthChecker {
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

// checkAll checks health of all backends
func (hc *HealthChecker) checkAll() {
	backends := hc.pool.GetBackends()
	for _, backend := range backends {
		hc.checkBackend(backend)
	}
}

// checkBackend checks health of a single backend
func (hc *HealthChecker) checkBackend(backend *Backend) {
	// Health check endpoint: /v1/models
	healthURL := backend.URL.String() + "/v1/models"

	resp, err := hc.client.Get(healthURL)
	wasHealthy := backend.IsHealthy()

	if err != nil {
		// Connection error
		backend.SetHealthy(false)
		if wasHealthy {
			log.Printf("[HEALTH] %s marked as unhealthy (error: %v)", backend.URL.String(), err)
		}
		return
	}
	defer resp.Body.Close()

	// Check if response is 2xx
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		backend.SetHealthy(true)
		if !wasHealthy {
			log.Printf("[HEALTH] %s marked as healthy", backend.URL.String())
		}
	} else {
		backend.SetHealthy(false)
		if wasHealthy {
			log.Printf("[HEALTH] %s marked as unhealthy (status: %d)", backend.URL.String(), resp.StatusCode)
		}
	}
}
