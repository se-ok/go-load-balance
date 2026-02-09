# Testing Guide for go-load-balance

This guide will walk you through testing the load balancer with mock backends.

## Prerequisites

- Go installed (for building)
- `curl` and `jq` installed (for testing)
- Terminal with bash support

## Step 1: Build Everything

```bash
# Build the load balancer
go build -o go-load-balance

# Build the mock backend
go build -o mock-backend mock-backend.go
```

## Step 2: Start Mock Backends

The mock backend supports various modes for testing different scenarios.

### Scenario A: Three Healthy Backends

Open 3 terminals and run:

```bash
# Terminal 1
./mock-backend --port 8000 --mode healthy

# Terminal 2
./mock-backend --port 8001 --mode healthy

# Terminal 3
./mock-backend --port 8002 --mode healthy
```

### Scenario B: Mixed Healthy and Slow Backends

```bash
# Terminal 1: Healthy, fast responses
./mock-backend --port 8000 --mode healthy

# Terminal 2: Healthy, slow responses (500ms delay)
./mock-backend --port 8001 --mode healthy --delay 500ms

# Terminal 3: Healthy, very slow (5 second delay)
./mock-backend --port 8002 --mode healthy --delay 5s
```

### Scenario C: One Failing Backend

```bash
# Terminal 1: Healthy
./mock-backend --port 8000 --mode healthy

# Terminal 2: Healthy
./mock-backend --port 8001 --mode healthy

# Terminal 3: Always fails
./mock-backend --port 8002 --mode failing
```

### Scenario D: Flaky Backend

```bash
# Terminal 1: Healthy
./mock-backend --port 8000 --mode healthy

# Terminal 2: Flaky (50% failure rate)
./mock-backend --port 8001 --mode flaky --failure-rate 0.5

# Terminal 3: Healthy
./mock-backend --port 8002 --mode healthy
```

## Step 3: Start Load Balancer

In a new terminal:

```bash
# Basic start (no verbose logging)
./go-load-balance --backends http://localhost:8000 http://localhost:8001 http://localhost:8002

# With verbose logging
./go-load-balance --backends http://localhost:8000 http://localhost:8001 http://localhost:8002 --verbose

# Using bash expansion
./go-load-balance --backends http://localhost:800{0..2} --verbose
```

## Step 4: Run Tests

### Manual Testing

```bash
# Test health endpoint
curl http://localhost:8080/v1/models | jq .

# Test completions
curl http://localhost:8080/v1/completions \
  -H "Content-Type: application/json" \
  -d '{"model": "test", "prompt": "Hello", "max_tokens": 10}' | jq .

# The response will include "backend_port" showing which backend handled the request
```

### Automated Basic Test

```bash
./test-basic.sh
```

This will:
1. Check if load balancer is running
2. Test health endpoint
3. Send a single completion request
4. Send 10 sequential requests to observe load distribution
5. Send 20 concurrent requests

### Test Load Balancing Distribution

```bash
# Send 100 requests and count distribution
for i in {1..100}; do
  curl -s http://localhost:8080/v1/completions \
    -H "Content-Type: application/json" \
    -d '{"prompt": "test"}' | jq -r '.backend_port'
done | sort | uniq -c
```

Expected output (approximately even distribution):
```
  34 8000
  33 8001
  33 8002
```

## Step 5: Test Health Checking

### Test Health Recovery

1. Start all backends healthy
2. Start load balancer with verbose mode
3. Stop one backend (Ctrl+C in its terminal)
4. Wait 30 seconds and watch load balancer logs - should show backend marked as unhealthy
5. Restart the backend
6. Wait 30 seconds - should show backend marked as healthy again

### Expected Logs

```
[HEALTH] http://localhost:8001 marked as unhealthy (error: connection refused)
[STATUS] Active: 5 | Healthy: 2/3
...
[HEALTH] http://localhost:8001 marked as healthy
[STATUS] Active: 5 | Healthy: 3/3
```

## Step 6: Test Different Scenarios

### Large Request Bodies

```bash
# Generate 10MB payload
python3 -c "import json; print(json.dumps({'data': 'x' * 10000000}))" > large-payload.json

# Send large request
curl -X POST http://localhost:8080/v1/completions \
  -H "Content-Type: application/json" \
  --data-binary @large-payload.json
```

### Large Response Bodies

```bash
# Start backend with large responses (10MB)
./mock-backend --port 8000 --mode healthy --response-size 10485760

# Request will return large response
curl -s http://localhost:8080/v1/completions \
  -H "Content-Type: application/json" \
  -d '{"prompt": "test"}' | wc -c
```

### Concurrent Load Test

```bash
# Send 1000 concurrent requests
seq 1 1000 | xargs -n1 -P100 -I{} curl -s http://localhost:8080/v1/completions \
  -H "Content-Type: application/json" \
  -d '{"prompt": "test"}' > /dev/null

# Check load balancer logs for status
```

### Timeout Testing

```bash
# Start backend with very long delay
./mock-backend --port 8000 --mode timeout

# Start load balancer with short timeout
./go-load-balance --backends http://localhost:8000 --timeout 5s

# Request should timeout after 5 seconds
curl http://localhost:8080/v1/completions \
  -H "Content-Type: application/json" \
  -d '{"prompt": "test"}'
```

## Expected Behaviors

### Random Two-Least Selection
- Requests distributed relatively evenly across healthy backends
- Backends with fewer active connections preferred (visible in verbose logs)
- Not perfectly round-robin (has randomness)

### Health Checking
- Health checks run every 30 seconds (configurable)
- Unhealthy backends automatically removed from rotation
- Backends can recover and be re-added to rotation

### Status Logging
- Without `--verbose`: Shows only summary every 30s
- With `--verbose`: Shows per-backend details every 30s
- Active connection counts should be accurate

## Mock Backend Modes

| Mode | Behavior |
|------|----------|
| `healthy` | Always returns 200 OK |
| `slow` | Returns 200 OK after `--delay` duration |
| `failing` | Always returns 500 error |
| `flaky` | Randomly succeeds or fails based on `--failure-rate` |
| `timeout` | Never responds (sleeps forever) |

## Mock Backend Options

```
--port <int>              Port to listen on (default: 8000)
--mode <string>           Mode: healthy, slow, failing, flaky, timeout (default: healthy)
--delay <duration>        Response delay (e.g., 100ms, 5s, 10m)
--failure-rate <float>    Failure rate for flaky mode (0.0-1.0, default: 0.5)
--response-size <int>     Response body size in bytes (default: 1024)
```

## Troubleshooting

### Load balancer says "no healthy backends available"
- Check that mock backends are running
- Check backend logs for health check requests
- Wait 30 seconds for health check to run
- Verify backends are responding on `/v1/models` endpoint

### Requests not distributed evenly
- This is expected with random two-least algorithm
- Distribution should be approximately even over many requests
- Check verbose logs to see active connection counts

### Connection refused errors
- Verify backend ports match what load balancer expects
- Check that backends are actually running (ps aux | grep mock-backend)

## Clean Up

```bash
# Kill all processes
pkill -f go-load-balance
pkill -f mock-backend
```
