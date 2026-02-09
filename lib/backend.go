package lib

import (
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync"
)

// Backend represents a single backend server
type Backend struct {
	URL          *url.URL
	proxy        *httputil.ReverseProxy
	mu           sync.Mutex
	healthy      bool
	activeConns  int
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
		log.Printf("[HEALTH] %s marked as unhealthy (proxy error: %v)", u.String(), err)
		b.SetHealthy(false)
		w.WriteHeader(http.StatusBadGateway)
	}

	// Mark backend unhealthy on non-2xx responses
	b.proxy.ModifyResponse = func(resp *http.Response) error {
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			log.Printf("[HEALTH] %s marked as unhealthy (status: %d)", u.String(), resp.StatusCode)
			b.SetHealthy(false)
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

// SetHealthy sets the backend health status
func (b *Backend) SetHealthy(healthy bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.healthy = healthy
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
