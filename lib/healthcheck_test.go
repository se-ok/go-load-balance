package lib

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// probeStatus runs one health check against a server answering /v1/models
// with the given status and reports the backend's resulting health.
func probeStatus(t *testing.T, status int, startHealthy bool) bool {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
	}))
	defer srv.Close()

	pool, err := NewPool([]string{srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	backend := pool.backends[0]
	if !startHealthy {
		backend.MarkUnhealthy()
		// recovery hysteresis: one passing probe is not enough, so run two
	}

	hc := NewHealthChecker(pool, 5*time.Second)
	hc.checkBackend(backend)
	hc.checkBackend(backend)
	return backend.IsHealthy()
}

func TestHealthProbeStatuses(t *testing.T) {
	if !probeStatus(t, http.StatusOK, true) {
		t.Error("200 probe should keep a backend healthy")
	}
	if !probeStatus(t, http.StatusTooManyRequests, true) {
		t.Error("429 probe (saturated but alive) must NOT eject a backend")
	}
	if !probeStatus(t, http.StatusTooManyRequests, false) {
		t.Error("429 probes should count toward recovery")
	}
	if probeStatus(t, http.StatusServiceUnavailable, true) {
		t.Error("503 probe should mark a backend unhealthy")
	}
	if probeStatus(t, http.StatusNotFound, true) {
		t.Error("404 probe should mark a backend unhealthy")
	}
}

func TestHealthProbeConnectionError(t *testing.T) {
	pool, err := NewPool([]string{"http://127.0.0.1:1"}) // nothing listens here
	if err != nil {
		t.Fatal(err)
	}
	hc := NewHealthChecker(pool, 5*time.Second)
	hc.checkBackend(pool.backends[0])
	if pool.backends[0].IsHealthy() {
		t.Error("connection-refused probe should mark a backend unhealthy")
	}
}

func TestHealthCheckSweepIsConcurrent(t *testing.T) {
	// Two hanging backends probed sequentially would take 2x the probe
	// timeout; concurrently they finish in ~one. Use a short-interval checker
	// (probe timeout = interval - 0.5s floor-clamped to 4.5s).
	var inFlight, peak atomic.Int32
	slow := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cur := inFlight.Add(1)
		defer inFlight.Add(-1)
		for {
			old := peak.Load()
			if cur <= old || peak.CompareAndSwap(old, cur) {
				break
			}
		}
		time.Sleep(300 * time.Millisecond)
	})
	s1, s2 := httptest.NewServer(slow), httptest.NewServer(slow)
	defer s1.Close()
	defer s2.Close()

	pool, err := NewPool([]string{s1.URL, s2.URL})
	if err != nil {
		t.Fatal(err)
	}
	NewHealthChecker(pool, 5*time.Second).checkAll()
	if peak.Load() < 2 {
		t.Errorf("expected concurrent probes, peak in-flight was %d", peak.Load())
	}
}
