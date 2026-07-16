package lib

import (
	"errors"
	"math"
	"math/rand"
	"net/http"
	"sync"
)

var (
	errNoHealthyBackends = errors.New("no healthy backends available")
	errAtCapacity        = errors.New("all healthy backends at max connections")
)

// writeSelectError maps selection failures to responses. At-capacity is
// backpressure, not an outage, so it is reported as a provider-style 429 —
// crucially a 4xx, which an upstream lb (two-tier deployments) passes through
// without marking this instance's node unhealthy. No healthy backends is a
// real outage: 503.
func writeSelectError(w http.ResponseWriter, err error) {
	if errors.Is(err, errAtCapacity) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"Rate limit reached: all backends at max concurrent requests, please retry later.","type":"rate_limit_error","code":"rate_limit_exceeded"}}`))
		return
	}
	http.Error(w, "Service Unavailable: "+err.Error(), http.StatusServiceUnavailable)
}

// Pool manages a collection of backends
type Pool struct {
	backends []*Backend
	mu       sync.RWMutex
	// maxConns caps concurrent proxied requests per backend (0 = unlimited).
	// Backends at the cap are skipped by selection; if every healthy backend
	// is at the cap the request is rejected with 503 (hard limit, no queue).
	maxConns int
	// affinity is non-nil in cache-aware routing mode (see cacheaware.go)
	affinity *affinityState
	// reqlog is non-nil when --log-to is set (see reqlog.go)
	reqlog *RequestLog
}

// SetRequestLog enables request/response pair logging (--log-to).
// Call before serving traffic.
func (p *Pool) SetRequestLog(l *RequestLog) {
	p.reqlog = l
}

// SetMaxConns sets the per-backend concurrent request cap (0 = unlimited).
// Call before serving traffic.
func (p *Pool) SetMaxConns(n int) {
	p.maxConns = n
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

// leastConnLocked returns the healthy backend with the fewest active
// connections (random tie-break) and its index, skipping backends at the
// maxConns cap. Callers must hold p.mu.
func (p *Pool) leastConnLocked() (*Backend, int, error) {
	minConns := math.MaxInt
	var least []*Backend
	var leastIdx []int
	anyHealthy := false
	for i, b := range p.backends {
		if !b.IsHealthy() {
			continue
		}
		anyHealthy = true
		c := b.GetActiveConns()
		if p.maxConns > 0 && c >= p.maxConns {
			continue
		}
		switch {
		case c < minConns:
			minConns = c
			least = append(least[:0], b)
			leastIdx = append(leastIdx[:0], i)
		case c == minConns:
			least = append(least, b)
			leastIdx = append(leastIdx, i)
		}
	}

	if len(least) == 0 {
		if anyHealthy {
			return nil, -1, errAtCapacity
		}
		return nil, -1, errNoHealthyBackends
	}

	k := rand.Intn(len(least)) // #nosec G404 -- tie-break among equally loaded backends, not security-sensitive
	return least[k], leastIdx[k], nil
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

	backend, _, err := p.leastConnLocked()
	if err != nil {
		return nil, err
	}
	backend.IncrementConns()
	return backend, nil
}

// ServeHTTP implements http.Handler interface
func (p *Pool) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var rec *reqLogCapture
	if p.reqlog != nil {
		rec, w = p.reqlog.begin(w, r)
		defer rec.finish()
	}

	if p.affinity != nil {
		p.serveCacheAware(w, r, rec)
		return
	}

	backend, err := p.SelectBackend()
	if err != nil {
		writeSelectError(w, err)
		return
	}
	rec.setBackend(backend)

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
