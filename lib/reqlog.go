package lib

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"sync"
	"time"
)

// Request/response logging (--log-to): every request handled by the pool is
// appended to a JSON Lines file as one object pairing the request body with
// the response body. Bodies are tee'd as they stream, so long-lived SSE
// responses are captured without buffering the proxy path.

// reqLogMaxCapture bounds how many bytes of a request or response body are
// retained in memory (and logged) per request — a DoS guard only, matching
// affinityMaxBody; real LLM traffic never approaches it. The stream itself
// keeps flowing past the cap; only the logged copy is cut off, flagged with
// request_truncated/response_truncated.
const reqLogMaxCapture = 1 << 30 // 1 GiB

// RequestLog appends request/response pairs to a JSONL file.
type RequestLog struct {
	mu sync.Mutex
	f  *os.File
	// failed suppresses repeated write-error logging until a write succeeds
	failed bool
}

// NewRequestLog opens path for appending, creating it if needed.
func NewRequestLog(path string) (*RequestLog, error) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o640) // #nosec G302 G304 -- path is the operator's --log-to flag; group-readable so log shippers can collect it
	if err != nil {
		return nil, err
	}
	return &RequestLog{f: f}, nil
}

// Close closes the underlying file.
func (l *RequestLog) Close() error {
	return l.f.Close()
}

// reqLogEntry is the JSON shape of one logged pair. Request and Response hold
// the raw body when it is valid JSON (the normal case for OpenAI-style
// requests), the body as a string otherwise (e.g. SSE streams), or null when
// empty.
type reqLogEntry struct {
	Time              time.Time `json:"time"`
	DurationMs        int64     `json:"duration_ms"`
	Method            string    `json:"method"`
	Path              string    `json:"path"`
	Status            int       `json:"status"`
	Backend           string    `json:"backend,omitempty"`
	Request           any       `json:"request"`
	RequestTruncated  bool      `json:"request_truncated,omitempty"`
	Response          any       `json:"response"`
	ResponseTruncated bool      `json:"response_truncated,omitempty"`
}

func (l *RequestLog) write(e *reqLogEntry) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(e); err != nil {
		log.Printf("[REQLOG] failed to encode entry: %v", err)
		return
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	if _, err := l.f.Write(buf.Bytes()); err != nil {
		if !l.failed {
			log.Printf("[REQLOG] failed to write entry: %v", err)
		}
		l.failed = true
		return
	}
	l.failed = false
}

// capBuffer captures up to reqLogMaxCapture bytes. Write never fails and
// always reports the full length, so a TeeReader through it never disturbs
// the stream it observes. It is locked because the transport may still be
// draining the request body from its write loop when the capture is
// finalized.
type capBuffer struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	truncated bool
	// limit overrides reqLogMaxCapture when > 0 (tests only)
	limit int
}

func (c *capBuffer) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	limit := c.limit
	if limit == 0 {
		limit = reqLogMaxCapture
	}
	if room := limit - c.buf.Len(); room < len(p) {
		c.truncated = true
		if room > 0 {
			c.buf.Write(p[:room])
		}
	} else {
		c.buf.Write(p)
	}
	return len(p), nil
}

func (c *capBuffer) snapshot() ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return bytes.Clone(c.buf.Bytes()), c.truncated
}

// reqLogCapture accumulates one request/response pair. Methods are nil-safe
// so call sites don't branch on whether logging is enabled.
type reqLogCapture struct {
	log     *RequestLog
	start   time.Time
	method  string
	path    string
	backend string
	status  int
	reqBuf  capBuffer
	respBuf capBuffer
}

type teeReadCloser struct {
	io.Reader
	io.Closer
}

// begin wires capture into a request: the body is tee'd into the capture as
// the proxy consumes it, and the returned writer records status and response
// bytes. The caller must defer finish() on the returned capture.
func (l *RequestLog) begin(w http.ResponseWriter, r *http.Request) (*reqLogCapture, http.ResponseWriter) {
	c := &reqLogCapture{
		log:    l,
		start:  time.Now(),
		method: r.Method,
		path:   r.URL.RequestURI(),
	}
	if r.Body != nil && r.Body != http.NoBody {
		r.Body = teeReadCloser{io.TeeReader(r.Body, &c.reqBuf), r.Body}
	}
	return c, &logResponseWriter{ResponseWriter: w, c: c}
}

// setBackend records which backend serves the request.
func (c *reqLogCapture) setBackend(b *Backend) {
	if c == nil {
		return
	}
	c.backend = b.URL.String()
}

// finish writes the accumulated pair as one JSONL line.
func (c *reqLogCapture) finish() {
	if c == nil {
		return
	}
	status := c.status
	if status == 0 {
		// Handler returned without writing anything; net/http sends 200.
		status = http.StatusOK
	}
	reqBody, reqTrunc := c.reqBuf.snapshot()
	respBody, respTrunc := c.respBuf.snapshot()
	c.log.write(&reqLogEntry{
		Time:              c.start.UTC(),
		DurationMs:        time.Since(c.start).Milliseconds(),
		Method:            c.method,
		Path:              c.path,
		Status:            status,
		Backend:           c.backend,
		Request:           bodyValue(reqBody),
		RequestTruncated:  reqTrunc,
		Response:          bodyValue(respBody),
		ResponseTruncated: respTrunc,
	})
}

// bodyValue embeds a captured body in the log entry: raw when valid JSON,
// as a string otherwise, nil when empty.
func bodyValue(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	if json.Valid(b) {
		return json.RawMessage(b)
	}
	return string(b)
}

// logResponseWriter records the status code and tees response bytes into the
// capture on their way to the client.
type logResponseWriter struct {
	http.ResponseWriter
	c *reqLogCapture
}

func (w *logResponseWriter) WriteHeader(code int) {
	if w.c.status == 0 {
		w.c.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *logResponseWriter) Write(p []byte) (int, error) {
	if w.c.status == 0 {
		w.c.status = http.StatusOK
	}
	w.c.respBuf.Write(p)
	return w.ResponseWriter.Write(p)
}

// Unwrap lets http.NewResponseController reach the underlying writer's Flush
// and deadline methods, which ReverseProxy needs to stream SSE responses.
func (w *logResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}
