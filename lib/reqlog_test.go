package lib

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestCapBufferTruncation(t *testing.T) {
	c := &capBuffer{limit: 10}

	n, err := c.Write([]byte("12345"))
	if n != 5 || err != nil {
		t.Fatalf("Write = (%d, %v), want (5, nil)", n, err)
	}
	// Crosses the limit: reports full length, keeps only what fits.
	n, err = c.Write([]byte("6789abcdef"))
	if n != 10 || err != nil {
		t.Fatalf("Write = (%d, %v), want (10, nil)", n, err)
	}
	got, truncated := c.snapshot()
	if string(got) != "123456789a" || !truncated {
		t.Fatalf("snapshot = (%q, %v), want (%q, true)", got, truncated, "123456789a")
	}
	// Already full: everything dropped, still no error.
	if n, err := c.Write([]byte("x")); n != 1 || err != nil {
		t.Fatalf("Write past cap = (%d, %v), want (1, nil)", n, err)
	}
}

func TestCapBufferNoTruncation(t *testing.T) {
	c := &capBuffer{limit: 10}
	if _, err := c.Write([]byte("1234567890")); err != nil {
		t.Fatal(err)
	}
	got, truncated := c.snapshot()
	if string(got) != "1234567890" || truncated {
		t.Fatalf("snapshot = (%q, %v), want exact content and no truncation", got, truncated)
	}
}

func TestBodyValue(t *testing.T) {
	if v := bodyValue(nil); v != nil {
		t.Fatalf("empty body = %v, want nil", v)
	}
	if v, ok := bodyValue([]byte(`{"a":1}`)).(json.RawMessage); !ok {
		t.Fatalf("JSON body = %T, want json.RawMessage", v)
	}
	if v, ok := bodyValue([]byte("data: {}\n\n")).(string); !ok {
		t.Fatalf("non-JSON body = %T, want string", v)
	}
}

// jsonEqual compares a decoded log field against a JSON literal, ignoring
// key order (decoding into `any` loses it).
func jsonEqual(t *testing.T, got any, want string) bool {
	t.Helper()
	var w any
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatal(err)
	}
	return reflect.DeepEqual(got, w)
}

// readLogEntries polls the JSONL file until it holds want entries (the
// deferred capture write races the client seeing the response).
func readLogEntries(t *testing.T, path string, want int) []reqLogEntry {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		data, err := os.ReadFile(path) // #nosec G304 -- test-owned temp path
		if err == nil {
			lines := strings.Split(strings.TrimSpace(string(data)), "\n")
			if len(data) > 0 && len(lines) >= want {
				entries := make([]reqLogEntry, len(lines))
				for i, line := range lines {
					if err := json.Unmarshal([]byte(line), &entries[i]); err != nil {
						t.Fatalf("line %d is not valid JSON: %v\n%s", i, err, line)
					}
				}
				return entries
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("log file did not reach %d entries in time", want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func newLoggedPool(t *testing.T, backendURL string) (*Pool, string) {
	t.Helper()
	pool, err := NewPool([]string{backendURL})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "pairs.jsonl")
	reqLog, err := NewRequestLog(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reqLog.Close() })
	pool.SetRequestLog(reqLog)
	return pool, path
}

func TestRequestLogPairsOverHTTP(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"hi"}}]}`))
	}))
	defer backend.Close()

	pool, path := newLoggedPool(t, backend.URL)
	lb := httptest.NewServer(pool)
	defer lb.Close()

	reqBody := `{"model":"m","messages":[{"role":"user","content":"hello"}]}`
	resp, err := http.Post(lb.URL+"/v1/chat/completions?x=1", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	e := readLogEntries(t, path, 1)[0]
	if e.Method != "POST" || e.Path != "/v1/chat/completions?x=1" || e.Status != http.StatusOK {
		t.Fatalf("entry = %s %s %d, want POST /v1/chat/completions?x=1 200", e.Method, e.Path, e.Status)
	}
	if e.Backend != backend.URL {
		t.Fatalf("backend = %q, want %q", e.Backend, backend.URL)
	}
	if !jsonEqual(t, e.Request, reqBody) {
		t.Fatalf("request = %#v, want %s", e.Request, reqBody)
	}
	respLogged, _ := json.Marshal(e.Response)
	if !strings.Contains(string(respLogged), `"hi"`) {
		t.Fatalf("response = %s, want the backend's JSON body", respLogged)
	}
	if e.RequestTruncated || e.ResponseTruncated {
		t.Fatal("unexpected truncation flags")
	}
	if e.DurationMs < 0 || e.Time.IsZero() {
		t.Fatalf("bad time/duration: %v / %d", e.Time, e.DurationMs)
	}
}

func TestRequestLogNonJSONResponseAsString(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"delta\":\"h\"}\n\ndata: [DONE]\n\n"))
	}))
	defer backend.Close()

	pool, path := newLoggedPool(t, backend.URL)
	lb := httptest.NewServer(pool)
	defer lb.Close()

	resp, err := http.Post(lb.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	e := readLogEntries(t, path, 1)[0]
	s, ok := e.Response.(string)
	if !ok || !strings.Contains(s, "[DONE]") {
		t.Fatalf("response = %#v, want the SSE stream as a string", e.Response)
	}
}

func TestRequestLogSelectErrors(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer backend.Close()

	pool, path := newLoggedPool(t, backend.URL)
	pool.GetBackends()[0].MarkUnhealthy()
	lb := httptest.NewServer(pool)
	defer lb.Close()

	resp, err := http.Post(lb.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	e := readLogEntries(t, path, 1)[0]
	if e.Status != http.StatusServiceUnavailable || e.Backend != "" {
		t.Fatalf("entry = status %d backend %q, want 503 with no backend", e.Status, e.Backend)
	}
}

func TestRequestLogCacheAwareMode(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer backend.Close()

	pool, path := newLoggedPool(t, backend.URL)
	pool.EnableCacheAware(time.Hour, 4)
	lb := httptest.NewServer(pool)
	defer lb.Close()

	reqBody := `{"messages":[{"role":"user","content":"hello"}]}`
	resp, err := http.Post(lb.URL+"/v1/chat/completions", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	e := readLogEntries(t, path, 1)[0]
	if e.Backend != backend.URL || e.Status != http.StatusOK {
		t.Fatalf("entry = status %d backend %q, want 200 %q", e.Status, e.Backend, backend.URL)
	}
	if !jsonEqual(t, e.Request, reqBody) {
		t.Fatalf("request = %#v, want %s", e.Request, reqBody)
	}
}
