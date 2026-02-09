# Go Load Balancer Plan - LLM Endpoint Load Balancer

## Purpose
A simple, lightweight HTTP load balancer specifically designed for managing multiple LLM generation endpoints (vLLM, SGLang, etc.) that provide OpenAI-compatible APIs.

## Key Requirements
- **CLI-first configuration**: No config files required, all configuration via command-line arguments
- **Simple deployment**: No special privileges needed (unlike nginx)
- **HTTP only (V1)**: Layer 7 load balancer for API endpoints (no HTTPS/TLS termination in V1)
- **Long-running requests**: Support request timeouts up to several hours (LLM generation can be slow)
- **No retry logic**: Let clients handle retries, load balancer only routes requests
- **Streaming-ready architecture**: Use httputil.ReverseProxy for transparent request/response handling

## Design Decisions

### 1. Load Balancing Algorithm
**Random Two-Least (Power of Two Choices)**
- For each request: randomly select 2 healthy backends, choose the one with fewer active connections
- Better load distribution than pure random
- Lower overhead than tracking all connections globally
- Similar to nginx's `least_conn` with random sampling

**Implementation details:**
- Track active connection count per backend (increment on request start, decrement on completion)
- Thread-safe counters using **mutex-protected int** (chosen for clarity and correctness)
- If only 1 healthy backend exists, use it directly

**Active Connection Counter: Mutex vs Atomic**

While `atomic.Int32` is more performant, we choose **mutex-protected int** for these reasons:

1. **Deferred decrement pattern**: Connection counts must be decremented even on panic. The idiomatic pattern is:
   ```go
   backend.mu.Lock()
   backend.activeConns++
   backend.mu.Unlock()
   defer func() {
       backend.mu.Lock()
       backend.activeConns--
       backend.mu.Unlock()
   }()
   ```
   This is clearer than atomic operations with defer.

2. **Less error-prone**: With atomic operations, it's easy to accidentally read the non-atomic way or forget to use atomic operations consistently. The mutex makes the critical section explicit.

3. **Readability for future maintainers**: Someone reading the code without context immediately understands:
   - `mu.Lock()` / `mu.Unlock()` = "this is thread-safe"
   - A naked variable access to `activeConns` without mutex = bug (easier to spot in code review)

   With atomics, both `atomic.LoadInt32(&activeConns)` and direct access to `activeConns` look valid, making bugs harder to catch.

4. **Grouped with health status**: Since health status already needs mutex protection (bool isn't atomic-safe for concurrent read/write), keeping both under the same mutex simplifies the design.

5. **Performance is not critical here**: Connection counter updates happen once per request (at start and end). This is not a hot path compared to the actual request/response proxying.

**Decision**: Use `sync.Mutex` with regular int for active connection counts.

### 2. Core Features for V1

#### Must Have:
1. **CLI Configuration**
   - `--backends` - Space-separated backend URLs (supports bash expansion): `http://localhost:800{0..2}` expands to `http://localhost:8000 http://localhost:8001 http://localhost:8002`
   - `--port` - Listen port (default: 8080)
   - `--timeout` - Request timeout duration (default: 4h for long LLM generation)
   - `--health-check-interval` - Health check interval (default: 30s)
   - `--verbose` - Enable verbose logging including per-backend breakdown (default: false)
   - No config file required

2. **HTTP Reverse Proxy (using httputil.ReverseProxy)**
   - Forward requests to backends using random two-least algorithm
   - Transparently proxy all requests/responses (no buffering)
   - Preserve all headers, methods, and body streams
   - Track active connections per backend
   - httputil.ReverseProxy handles:
     - Request forwarding
     - Response streaming
     - Header preservation
     - Chunked encoding
     - WebSocket upgrades (for future streaming support)

3. **Request Timeout**
   - Support very long timeouts (up to several hours) for LLM generation
   - Configurable via `--timeout` flag
   - Default: 4 hours (14400 seconds)
   - Applied to backend HTTP client transport

4. **Health Checks**
   - Periodic HTTP GET to health endpoint
   - Mark backends as healthy/unhealthy
   - Only route to healthy backends
   - Configurable check interval

5. **Logging**
   - Startup configuration summary
   - Health check status changes (when backend transitions healthy/unhealthy)
   - **Periodic status report (every 30 seconds):**
     - Always show: Total active connections across all backends, Number of healthy backends / total backends
     - With `--verbose` flag: Per-backend breakdown (URL, healthy status, active connections)

#### Nice to Have (Future):
- Graceful shutdown
- Detailed request logging with verbosity levels
- Metrics/statistics endpoint
- Custom health check endpoints per backend
- HTTPS/TLS termination (should be straightforward with standard library)
- SSE streaming support (httputil.ReverseProxy already handles this transparently)

### 3. Technical Architecture

```
┌─────────────┐
│   Client    │
└──────┬──────┘
       │
       ▼
┌──────────────────────────────────────┐
│   Go Load Balancer                   │
│   - HTTP Server (port 8080)          │
│   - Random Two-Least Selector        │
│   - Active Connection Tracking       │
│   - httputil.ReverseProxy            │
│   - Health Checker (30s interval)    │
│   - Request Timeout (4h default)     │
└──────┬───────────────────────────────┘
       │
       │ For each request:
       │ 1. Pick 2 random healthy backends
       │ 2. Select one with fewer active connections
       │ 3. Proxy request transparently (no buffering)
       │
       ├─────────────┬─────────────┐
       ▼             ▼             ▼
   ┌────────┐   ┌────────┐   ┌────────┐
   │ vLLM   │   │SGLang  │   │ vLLM   │
   │ :8000  │   │ :8001  │   │ :8002  │
   │ Conns:2│   │ Conns:5│   │ Conns:1│
   └────────┘   └────────┘   └────────┘
```

### 4. Go Project Structure

```
go-load-balance/
├── main.go              # Entry point, CLI parsing, server setup
├── go.mod               # Go module file
├── lb/
│   ├── balancer.go      # Core load balancer logic
│   ├── backend.go       # Backend server representation
│   └── healthcheck.go   # Health checking logic
└── README.md            # Usage documentation
```

### 5. Key Go Packages to Use
- `net/http` and `net/http/httputil` - HTTP server and reverse proxy
- `flag` - CLI argument parsing
- `sync` - Thread-safe backend state (using mutex for simplicity and correctness)
- `time` - Health check intervals and timeouts
- `log` - Simple, clear logging
- `math/rand` - Random selection for two-least algorithm

### 6. Architecture Philosophy

**Simplicity and Readability:**
- Keep the codebase small and focused
- Prefer clear, idiomatic Go code over clever optimizations
- Use standard library where possible
- Minimal abstractions - only add complexity when necessary
- Clear function names and well-structured code
- Comments for non-obvious logic only (code should be self-documenting)

**Key Principles:**
1. **Simple data structures**: Backend struct with minimal fields
2. **Clear separation**: Backend pool, health checker, proxy handler as distinct components
3. **Idiomatic concurrency**: Use goroutines naturally, mutex for thread-safe state (explicit and clear)
4. **Standard patterns**: Use `http.Handler` interface, standard context for cancellation
5. **No premature optimization**: Focus on correctness and clarity first
6. **Package naming**: Use short, clear package name `lb` (for load balancer)

### 6. Implementation Steps

1. **Project Setup**
   - Initialize Go module
   - Set up basic project structure

2. **Backend Management**
   - Create Backend struct with:
     - URL
     - Mutex (sync.Mutex)
     - Healthy status (bool, protected by mutex)
     - Active connection count (int, protected by mutex)
   - Implement backend pool with random two-least selection:
     - Get list of healthy backends (read lock)
     - If 0 healthy: return error
     - If 1 healthy: return that backend
     - If 2+ healthy: randomly pick 2, return one with fewer active connections
   - Thread-safe backend state management using mutex

3. **Reverse Proxy (No Retry Logic)**
   - Configure httputil.ReverseProxy for each backend
   - Implement request forwarding with:
     - Transparent proxying (no request/response buffering)
     - Active connection tracking (increment before, decrement after)
     - All headers, methods, and body streams passed through
   - No retry logic - client handles retries if needed
   - On backend error, return error directly to client

4. **Health Checks**
   - Background goroutine for periodic health checks
   - HTTP GET request to `/v1/models` endpoint
   - Mark backends as healthy/unhealthy based on response
   - Only include healthy backends in load balancing pool
   - Configurable check interval (default: 30s)
   - Log when backend transitions between healthy/unhealthy states

5. **Periodic Status Logging**
   - Background goroutine that logs status every 30 seconds
   - Basic log output format (always shown):
     ```
     [STATUS] Active: 12 | Healthy: 2/3
     ```
   - Verbose log output format (with `--verbose` flag):
     ```
     [STATUS] Active: 12 | Healthy: 2/3
     [STATUS]   http://localhost:8000 - healthy, 5 active
     [STATUS]   http://localhost:8001 - healthy, 7 active
     [STATUS]   http://localhost:8002 - unhealthy, 0 active
     ```

6. **CLI Interface**
   - Parse flags: `--backends` (space-separated), `--port`, `--timeout`, `--health-check-interval`, `--verbose`
   - Validate configuration (at least 1 backend, valid URLs, valid timeout)
   - Display startup summary
   - Note: Space-separated backends enable bash expansion (e.g., `http://localhost:800{0..2}`)

8. **HTTP Server**
   - Start HTTP server on specified port
   - Route all requests through load balancer

9. **Testing & Documentation**
   - Test with sample backends
   - Test load balancing algorithm
   - Test long-running requests (timeout handling)
   - Write README with usage examples

### 7. Example Usage

```bash
# Full configuration with verbose logging
./go-load-balance \
  --backends http://localhost:8000 http://localhost:8001 http://localhost:8002 \
  --port 8080 \
  --timeout 4h \
  --health-check-interval 30s \
  --verbose

# Using bash expansion (expands to: http://localhost:8000 http://localhost:8001 http://localhost:8002)
./go-load-balance --backends http://localhost:800{0..2}

# Minimal usage (uses defaults, no verbose logging)
./go-load-balance --backends http://localhost:8000 http://localhost:8001

# With custom timeout for faster responses
./go-load-balance \
  --backends http://gpu1:8000 http://gpu2:8000 \
  --timeout 30m
```

### 8. Finalized Design Decisions

1. **Health check endpoint**: ✅ Use `/v1/models` (OpenAI API standard)
   - Periodic HTTP GET to `{backend_url}/v1/models`
   - 2xx response = healthy, anything else = unhealthy

2. **No retry logic**: ✅ Client handles retries
   - Load balancer simply routes requests to selected backend
   - On error, return error directly to client
   - Client can retry if needed (more flexible and correct)
   - Rationale: Retrying at load balancer complicates logic and may duplicate work

3. **Request/Response handling**: ✅ Transparent proxying using httputil.ReverseProxy
   - No request body buffering (stream directly to backend)
   - No response body buffering (stream directly to client)
   - Minimal memory footprint
   - Automatically handles streaming, chunked encoding, WebSockets

## Current Status

Plan finalized with:
- ✅ Random two-least load balancing algorithm
- ✅ **No retry logic** - client-side responsibility
- ✅ Long request timeout support (default: 4h)
- ✅ HTTP-only (V1) - HTTPS/TLS can be added later with minimal changes
- ✅ CLI-first configuration
- ✅ Health checks via `/v1/models`
- ✅ **Transparent proxying** - no buffering, streams everything
- ✅ Periodic status logging (every 30s): active connections + healthy backend count
- ✅ **Streaming-ready** - httputil.ReverseProxy handles SSE/WebSockets transparently

## Future Extensions

### Adding HTTPS Support
Using `httputil.ReverseProxy` makes adding HTTPS straightforward:
- For backends using HTTPS: Just use `https://` URLs, no code changes needed
- For TLS termination (HTTPS on frontend): Add `--tls-cert` and `--tls-key` flags, use `http.ListenAndServeTLS()` instead of `http.ListenAndServe()`
- Minimal changes required (~10 lines of code)

### Adding Streaming Support
Already supported! `httputil.ReverseProxy` transparently handles:
- Server-Sent Events (SSE) - for OpenAI streaming completions
- WebSocket upgrades - for bidirectional streaming
- Chunked transfer encoding
- No code changes needed for basic streaming support

---

# Test Plan

## Test Environment Setup

### Mock Backend Servers
Create simple HTTP servers to simulate LLM backends with different behaviors:

1. **Healthy backend** - Returns 200 OK, sleeps for configurable duration
2. **Slow backend** - Returns 200 OK after long delay (for timeout testing)
3. **Failing backend** - Returns 500 Internal Server Error
4. **Flaky backend** - Randomly returns 200 or 500
5. **Unreachable backend** - Connection refused (server not running)

### Test Tools
- `curl` or `httpie` for sending requests
- Custom Go test script for concurrent request generation
- Logs inspection for verification

---

## Test Cases

### 1. Basic Functionality Tests

#### TC1.1: Startup and Configuration
**Objective:** Verify load balancer starts correctly with valid configuration

**Steps:**
1. Start load balancer with 3 backends:
   ```bash
   ./go-load-balance --backends http://localhost:8001 http://localhost:8002 http://localhost:8003 --port 8080
   ```

**Expected:**
- Load balancer starts successfully
- Logs show startup configuration summary
- All flags displayed with values (defaults where not specified)
- Health checks begin immediately

**Verification:**
- Check startup logs contain backend URLs, port, timeout settings
- No error messages

---

#### TC1.2: Invalid Configuration Rejection
**Objective:** Verify invalid configurations are rejected

**Test cases:**
- No backends specified: `./go-load-balance --port 8080`
- Invalid URL: `./go-load-balance --backends not-a-url`
- Invalid timeout: `./go-load-balance --backends http://localhost:8001 --timeout -5s`
- Invalid port: `./go-load-balance --backends http://localhost:8001 --port 99999`

**Expected:**
- Load balancer exits with error message
- Clear explanation of what's wrong

---

### 2. Load Balancing Algorithm Tests

#### TC2.1: Random Two-Least Selection
**Objective:** Verify random two-least algorithm distributes load correctly

**Setup:**
- Start 3 healthy backends
- Send 100 concurrent requests

**Steps:**
1. Start load balancer with 3 backends
2. Use script to send 100 requests concurrently
3. Monitor which backends receive requests

**Expected:**
- All backends receive approximately equal number of requests (within reasonable variance)
- Backends with fewer active connections are preferred when randomly selected
- Distribution is not perfectly round-robin (some randomness expected)

**Verification:**
- Check periodic status logs to see active connection counts
- Backends should have relatively balanced load

---

#### TC2.2: Single Healthy Backend
**Objective:** Verify behavior when only one backend is healthy

**Setup:**
- Start 3 backends: 1 healthy, 2 unhealthy

**Steps:**
1. Send 10 requests to load balancer
2. Check which backend handles them

**Expected:**
- All requests go to the single healthy backend
- No errors returned to client
- Periodic logs show 1/3 healthy backends

---

#### TC2.3: No Healthy Backends
**Objective:** Verify behavior when all backends are unhealthy

**Setup:**
- Start 3 backends, all returning 500 errors or unreachable

**Steps:**
1. Wait for health checks to mark all backends unhealthy
2. Send request to load balancer

**Expected:**
- Request fails with error (no healthy backends available)
- Client receives appropriate error response (502/503)

---

### 3. Health Check Tests

#### TC3.1: Health Check Detection
**Objective:** Verify health checks detect unhealthy backends

**Setup:**
- Start 3 healthy backends
- After 1 minute, make backend #2 return 500 on `/v1/models`

**Steps:**
1. Monitor logs for health check status
2. Wait for health check interval to pass

**Expected:**
- Initially all backends healthy (3/3)
- After backend #2 fails, logs show transition to unhealthy
- Periodic logs show 2/3 healthy backends
- No requests routed to unhealthy backend

---

#### TC3.2: Health Check Recovery
**Objective:** Verify backends can recover from unhealthy state

**Setup:**
- Start with 1 unhealthy backend
- After 1 minute, make it healthy (return 200 on `/v1/models`)

**Steps:**
1. Monitor health check logs
2. Wait for recovery

**Expected:**
- Initially logs show backend unhealthy
- After recovery, logs show transition to healthy
- Backend starts receiving requests again

---

#### TC3.3: Custom Health Check Interval
**Objective:** Verify configurable health check interval works

**Steps:**
1. Start load balancer with `--health-check-interval 10s`
2. Monitor logs for health check frequency

**Expected:**
- Health checks occur every 10 seconds (not default 30s)
- Logs show timestamps confirming interval

---

### 4. Timeout Tests

#### TC4.1: Long Request Timeout (4h default)
**Objective:** Verify long timeout works for slow LLM generation

**Setup:**
- Backend with 10-minute response delay
- Default timeout (4h)

**Steps:**
1. Send request to load balancer
2. Wait for response

**Expected:**
- Request does NOT timeout
- After 10 minutes, response is returned
- Client receives 200 OK

---

#### TC4.2: Custom Timeout
**Objective:** Verify custom timeout configuration

**Setup:**
- Backend with 2-minute response delay
- Timeout set to `--timeout 1m`

**Steps:**
1. Send request to load balancer
2. Wait for timeout

**Expected:**
- Request times out after 1 minute
- Client receives timeout error
- No retry attempted

---

### 5. Logging Tests

#### TC5.1: Periodic Status Logs (Every 30s)
**Objective:** Verify status logs are printed every 30 seconds

**Setup:**
- 3 backends: 2 healthy, 1 unhealthy
- Send some concurrent requests to generate active connections

**Steps:**
1. Start load balancer
2. Generate ongoing traffic
3. Monitor logs for 2 minutes

**Expected:**
- Without `--verbose`: Logs printed every 30 seconds with format:
  ```
  [STATUS] Active: 5 | Healthy: 2/3
  ```
- With `--verbose`: Logs include per-backend breakdown:
  ```
  [STATUS] Active: 5 | Healthy: 2/3
  [STATUS]   http://localhost:8001 - healthy, 2 active
  [STATUS]   http://localhost:8002 - healthy, 3 active
  [STATUS]   http://localhost:8003 - unhealthy, 0 active
  ```
- Active connection counts accurate
- Healthy backend count accurate

---

#### TC5.2: Health Transition Logs
**Objective:** Verify logs when backend health changes

**Setup:**
- Start with all healthy backends
- Make one backend fail

**Steps:**
1. Monitor logs
2. Stop backend #2

**Expected:**
- Log message when backend transitions to unhealthy:
  ```
  [HEALTH] http://localhost:8002 marked as unhealthy
  ```

**Verification:**
- Clear log message for state transition
- Timestamp included

---

### 6. Concurrent Load Tests

#### TC6.1: High Concurrent Requests (10K)
**Objective:** Verify load balancer handles very high concurrency

**Setup:**
- 3 healthy backends
- Generate 10,000 concurrent requests

**Steps:**
1. Use load testing tool (e.g., `wrk`, `ab`, or custom Go script)
2. Send 10,000 concurrent requests
3. Monitor status logs, memory usage, CPU usage
4. Monitor goroutine count

**Expected:**
- All requests eventually complete
- No crashes, panics, or deadlocks
- No goroutine leaks (goroutine count returns to baseline after load)
- No memory leaks (memory usage stable)
- Active connection counts in logs fluctuate appropriately
- Backends receive relatively balanced load

**Verification:**
- No errors in logs
- All client requests succeed (or fail gracefully if backends overwhelmed)
- Periodic status logs show reasonable connection distribution
- Resource usage (CPU, memory, goroutines) stable

---

#### TC6.2: Large Request Bodies (10MB per request)
**Objective:** Verify buffering and handling of large request bodies

**Setup:**
- 3 healthy backends
- 100 concurrent POST requests, each with 10MB JSON payload
- Total data: ~1GB

**Steps:**
1. Generate 10MB JSON payload (large array or nested structure)
2. Send 100 concurrent requests with this payload
3. Monitor memory usage, status logs

**Expected:**
- All requests complete successfully
- Request bodies streamed directly to backends (no buffering)
- Memory usage remains stable (streaming works correctly)
- Backends receive full 10MB payload

**Verification:**
- All requests succeed
- Backend logs confirm 10MB bodies received
- Memory usage doesn't spike excessively (streaming works)
- No OOM (Out Of Memory) errors

**Note:** This tests transparent streaming of large request bodies. Memory usage should be minimal since bodies are streamed, not buffered.

---

#### TC6.3: Large Response Bodies (10MB per response)
**Objective:** Verify handling of large response bodies from backends

**Setup:**
- 3 healthy backends configured to return 10MB response bodies
- 100 concurrent requests

**Steps:**
1. Configure backends to return 10MB JSON response
2. Send 100 concurrent requests
3. Monitor memory usage, response streaming

**Expected:**
- All responses received completely by clients
- Load balancer properly streams large responses (not buffering entire response)
- Memory usage stable (responses streamed, not buffered)
- No timeout errors on large responses

**Verification:**
- All clients receive full 10MB responses
- Response data integrity (correct content)
- Memory usage doesn't spike excessively (streaming works)

**Note:** Unlike request bodies (which must be buffered for retries), response bodies should be streamed directly to client without buffering.

---

#### TC6.4: Mixed Long and Short Requests
**Objective:** Verify handling of mixed request durations

**Setup:**
- 3 backends
- Send mix of fast (100ms) and slow (5 minute) requests

**Steps:**
1. Start 10 slow requests
2. While slow requests running, send 100 fast requests
3. Monitor status logs

**Expected:**
- Fast requests complete quickly
- Slow requests continue running
- Active connection counts reflect both types
- Fast requests balanced across backends with available capacity

---

### 7. Edge Cases

#### TC7.1: Backend Added/Removed During Runtime
**Objective:** Verify behavior when backends list is static (no dynamic changes)

**Note:** V1 doesn't support dynamic backend changes, so this is documentation of current limitation.

**Expected:**
- Backends list is static after startup
- To change backends, must restart load balancer

---

#### TC7.2: Very Large Request Body
**Objective:** Test buffering with large request bodies

**Setup:**
- Send POST with 5MB JSON body

**Steps:**
1. Send large request
2. Verify it's buffered and forwarded

**Expected:**
- Request succeeds
- Body streamed directly to backend

**Note:** Since we're not buffering request bodies, very large requests are handled efficiently via streaming

---

#### TC7.3: All Backends Timeout
**Objective:** Verify behavior when all backends timeout

**Setup:**
- 3 backends, all very slow (respond after 2 hours)
- Timeout set to 1 hour

**Steps:**
1. Send request
2. Wait for timeout

**Expected:**
- Request times out after 1 hour
- Client receives timeout error

---

## Test Execution Strategy

### Phase 1: Unit Tests (if applicable)
- Test individual components in isolation
- Backend selection algorithm
- Health check logic

### Phase 2: Integration Tests
- Test with real mock backends
- Run test cases TC1-TC7
- Verify all expected behaviors

### Phase 3: Performance/Load Tests
- Run concurrent load tests
- Verify stability under high load
- Check for memory leaks or goroutine leaks

### Phase 4: Edge Cases
- Test all edge cases
- Document any limitations

---

## Success Criteria

- ✅ All test cases pass
- ✅ No crashes or panics under normal operation
- ✅ No deadlocks or goroutine leaks
- ✅ Logs are clear and accurate
- ✅ Performance acceptable for target use case (LLM load balancing)
- ✅ Documentation complete (README with usage examples)

---

Ready for implementation approval.
