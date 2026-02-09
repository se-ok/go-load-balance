# go-load-balance

A simple, lightweight HTTP load balancer specifically designed for managing multiple LLM generation endpoints (vLLM, SGLang, etc.) that provide OpenAI-compatible APIs.

## Features

- **Random Two-Least Load Balancing**: Selects 2 random backends and routes to the one with fewer active connections
- **Health Checks**: Periodic health monitoring via `/v1/models` endpoint with transition logging
- **Long Request Support**: Default 4-hour timeout for slow LLM generation
- **CLI-First Configuration**: No config files needed, everything via command-line arguments
- **Bash Expansion Support**: Space-separated backends enable shell expansion
- **Transparent Proxying**: Streams requests and responses without buffering
- **Streaming Ready**: Supports SSE and WebSocket upgrades out of the box

## Installation

Requires Go 1.21+. If you don't have Go installed:

```bash
# Install Go without sudo (Linux amd64)
# See https://go.dev/dl/ for other platforms and latest versions
export GO_VERSION=1.23.6
curl -fsSL https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz | tar -C ~/.local -xz
export PATH=$HOME/.local/go/bin:$PATH
```

Build the load balancer:

```bash
go build -o lb ./cmd/lb
```

## Usage

### Basic Usage

```bash
./lb --backends http://localhost:8000 --backends http://localhost:8001 --backends http://localhost:8002
```

### Using Bash Expansion

```bash
./lb --backends http://localhost:800{0..2}
```

### Full Configuration

```bash
./lb \
  --backends http://localhost:8000 \
  --backends http://localhost:8001 \
  --backends http://localhost:8002 \
  --port 8080 \
  --timeout 4h \
  --health-check-interval 30s \
  --verbose
```

## Command-Line Arguments

| Flag | Description | Default |
|------|-------------|---------|
| `--backends` | Backend URL (required, repeat for multiple) | - |
| `--port` | Port to listen on | `8080` |
| `--timeout` | Request timeout duration | `4h` |
| `--health-check-interval` | Health check interval | `30s` |
| `--verbose` | Enable verbose logging with per-backend details | `false` |

## How It Works

1. **Load Balancing**: For each request, the load balancer randomly selects 2 healthy backends and routes to the one with fewer active connections
2. **Health Checks**: Every 30 seconds (configurable), the load balancer checks each backend's `/v1/models` endpoint
3. **Status Logging**: Every 30 seconds, logs current active connections and healthy backend count
4. **Transparent Proxying**: Uses Go's `httputil.ReverseProxy` to stream requests/responses without buffering
5. **No Healthy Backends**: When all backends are down, proxied requests return 502 Bad Gateway

## Architecture

```
┌─────────────┐
│   Client    │
└──────┬──────┘
       │
       ▼
┌──────────────────────────────────────┐
│   Go Load Balancer                   │
│   - Random Two-Least Selector        │
│   - Health Checker (30s)             │
│   - httputil.ReverseProxy            │
└──────┬───────────────────────────────┘
       │
       ├─────────────┬─────────────┐
       ▼             ▼             ▼
   ┌────────┐   ┌────────┐   ┌────────┐
   │ vLLM   │   │SGLang  │   │ vLLM   │
   │ :8000  │   │ :8001  │   │ :8002  │
   └────────┘   └────────┘   └────────┘
```

## Health Endpoint

The load balancer exposes `/health` which reports its own status without proxying to backends:

```bash
curl http://localhost:8080/health
# {"status":"ok","healthy_backends":3,"total_backends":3,"active_conns":5}
```

Returns 200 when at least one backend is healthy, 503 when all backends are down.

## Testing

Requires Python 3.10+ with `pytest`, `requests`, and `aiohttp`:

```bash
# Build binaries
go build -o lb ./cmd/lb
go build -o mock-backend ./cmd/mock-backend

# Run functional tests
pytest test.py -v

# Run stress test (also needs aiohttp)
python test_stress.py
```

## Project Structure

```
cmd/
  lb/              Load balancer entry point
  mock-backend/    Mock backend for testing
lib/               Core library (pool, balancer, health checker)
test.py            Functional tests (pytest)
test_stress.py     Stress test (asyncio + aiohttp)
```

## Design Choices

- **No retry logic**: The load balancer does not retry failed requests. On backend error, the error is returned directly to the client. Clients are responsible for their own retry strategy.
- **No request/response buffering**: Request and response bodies are streamed directly between client and backend, keeping memory usage minimal regardless of payload size.
- **Mutex over atomics**: Active connection counters use `sync.Mutex` rather than atomic operations for clarity and to group with health status under the same lock.

## Limitations

- **HTTP only**: No HTTPS/TLS termination. Place behind a TLS-terminating reverse proxy (e.g., nginx, caddy) if you need HTTPS on the frontend. Backends using `https://` URLs work without any changes.
- **Static backend list**: Backends are configured at startup and cannot be changed at runtime. Restart the load balancer to update the backend list.

## License

MIT
