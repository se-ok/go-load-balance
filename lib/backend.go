package lib

import (
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync"
)

// healthyThreshold is the number of consecutive successful health checks
// required before an unhealthy backend is marked healthy again. Recovering
// slowly (while failing fast) prevents a backend whose health endpoint
// responds but whose real requests fail from flapping in and out of the pool.
const healthyThreshold = 2

// Backend represents a single backend server
type Backend struct {
	URL         *url.URL
	proxy       *httputil.ReverseProxy
	mu          sync.Mutex
	healthy     bool
	activeConns int
	// consecutive successful health checks since the last failure
	successStreak int
}

// NewBackend creates a new Backend instance
func NewBackend(urlStr string) (*Backend, error) {
	u, err := url.Parse(urlStr)
	if err != nil {
		return nil, err
	}

	b := &Backend{
		URL:     u,
		proxy:   httputil.NewSingleHostReverseProxy(u),
		healthy: true, // Start as healthy, health checker will update
	}

	// Mark backend unhealthy immediately on proxy error, but only if the
	// error is from the backend (not the client dropping the connection).
	b.proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		if r.Context().Err() != nil {
			// Client cancelled — not the backend's fault
			log.Printf("[PROXY] %s client disconnected: %v", u.String(), err)
			return
		}
		if b.MarkUnhealthy() {
			log.Printf("[HEALTH] %s marked as unhealthy (proxy error: %v)", u.String(), err)
		}
		w.WriteHeader(http.StatusBadGateway)
	}

	// Mark backend unhealthy on 5xx responses. 4xx (including 429) are the
	// client's or the rate limiter's business, not a sign the backend is down.
	b.proxy.ModifyResponse = func(resp *http.Response) error {
		if resp.StatusCode >= 500 {
			if b.MarkUnhealthy() {
				log.Printf("[HEALTH] %s marked as unhealthy (status: %d)", u.String(), resp.StatusCode)
			}
		}
		return nil
	}

	return b, nil
}

// IsHealthy returns whether the backend is healthy
func (b *Backend) IsHealthy() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.healthy
}

// MarkUnhealthy marks the backend unhealthy and resets its recovery streak.
// It returns true if the backend was healthy before, i.e. this call was a
// state transition worth logging.
func (b *Backend) MarkUnhealthy() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	wasHealthy := b.healthy
	b.healthy = false
	b.successStreak = 0
	return wasHealthy
}

// RecordCheckSuccess records a successful health check. An unhealthy backend
// becomes healthy again only after healthyThreshold consecutive successes.
// It returns true if this call transitioned the backend to healthy.
func (b *Backend) RecordCheckSuccess() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.healthy {
		return false
	}
	b.successStreak++
	if b.successStreak < healthyThreshold {
		return false
	}
	b.healthy = true
	return true
}

// GetActiveConns returns the number of active connections
func (b *Backend) GetActiveConns() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.activeConns
}

// IncrementConns increments the active connection count
func (b *Backend) IncrementConns() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.activeConns++
}

// DecrementConns decrements the active connection count
func (b *Backend) DecrementConns() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.activeConns--
}

// GetProxy returns the reverse proxy for this backend
func (b *Backend) GetProxy() *httputil.ReverseProxy {
	return b.proxy
}
