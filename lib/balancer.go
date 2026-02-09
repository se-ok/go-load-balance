package lib

import (
	"errors"
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

// SelectBackend selects a backend using the random two-least algorithm
func (p *Pool) SelectBackend() (*Backend, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	// Get list of healthy backends
	healthy := make([]*Backend, 0, len(p.backends))
	for _, b := range p.backends {
		if b.IsHealthy() {
			healthy = append(healthy, b)
		}
	}

	// No healthy backends
	if len(healthy) == 0 {
		return nil, errors.New("no healthy backends available")
	}

	// Single healthy backend
	if len(healthy) == 1 {
		return healthy[0], nil
	}

	// Random two-least: pick 2 random backends, return one with fewer active connections
	idx1 := rand.Intn(len(healthy))
	idx2 := rand.Intn(len(healthy))

	// Ensure idx2 is different from idx1
	for idx2 == idx1 {
		idx2 = rand.Intn(len(healthy))
	}

	backend1 := healthy[idx1]
	backend2 := healthy[idx2]

	// Return backend with fewer active connections
	if backend1.GetActiveConns() <= backend2.GetActiveConns() {
		return backend1, nil
	}
	return backend2, nil
}

// ServeHTTP implements http.Handler interface
func (p *Pool) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	backend, err := p.SelectBackend()
	if err != nil {
		http.Error(w, "Service Unavailable: "+err.Error(), http.StatusServiceUnavailable)
		return
	}

	// Track active connections
	backend.IncrementConns()
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
