package lib

import (
	"errors"
	"math"
	"math/rand"
	"net/http"
	"sync"
)

// Pool manages a collection of backends
type Pool struct {
	backends []*Backend
	mu       sync.RWMutex
}

// NewPool creates a new backend pool
func NewPool(backendURLs []string) (*Pool, error) {
	if len(backendURLs) == 0 {
		return nil, errors.New("at least one backend is required")
	}

	backends := make([]*Backend, 0, len(backendURLs))
	for _, urlStr := range backendURLs {
		backend, err := NewBackend(urlStr)
		if err != nil {
			return nil, err
		}
		backends = append(backends, backend)
	}

	return &Pool{
		backends: backends,
	}, nil
}

// SelectBackend selects the healthy backend with the fewest active
// connections, breaking ties randomly, and reserves a connection slot on it
// before returning. Selection and increment happen under the pool lock, so
// concurrent selections each see the previous pick's slot and a simultaneous
// burst distributes within ±1 instead of herding onto one idle backend.
// The caller must release the slot with DecrementConns when done.
func (p *Pool) SelectBackend() (*Backend, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	minConns := math.MaxInt
	var least []*Backend
	for _, b := range p.backends {
		if !b.IsHealthy() {
			continue
		}
		switch c := b.GetActiveConns(); {
		case c < minConns:
			minConns = c
			least = append(least[:0], b)
		case c == minConns:
			least = append(least, b)
		}
	}

	if len(least) == 0 {
		return nil, errors.New("no healthy backends available")
	}

	backend := least[rand.Intn(len(least))]
	backend.IncrementConns()
	return backend, nil
}

// ServeHTTP implements http.Handler interface
func (p *Pool) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	backend, err := p.SelectBackend()
	if err != nil {
		http.Error(w, "Service Unavailable: "+err.Error(), http.StatusServiceUnavailable)
		return
	}

	// Connection slot was reserved by SelectBackend
	defer backend.DecrementConns()

	// Proxy the request
	backend.GetProxy().ServeHTTP(w, r)
}

// GetBackends returns all backends (for health checking and status logging)
func (p *Pool) GetBackends() []*Backend {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.backends
}

// GetStatus returns current pool status
func (p *Pool) GetStatus() (totalActive int, healthyCount int, totalCount int) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	totalCount = len(p.backends)
	for _, b := range p.backends {
		if b.IsHealthy() {
			healthyCount++
		}
		totalActive += b.GetActiveConns()
	}
	return
}
