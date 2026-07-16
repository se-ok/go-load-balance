# go-load-balance

A simple, lightweight HTTP load balancer specifically designed for managing multiple LLM generation endpoints (vLLM, SGLang, etc.) that provide OpenAI-compatible APIs.

## Features

- **Least-Connections Load Balancing**: Routes to the healthy backend with the fewest active connections, breaking ties randomly
- **Health Checks**: Periodic health monitoring via `/v1/models` endpoint with transition logging
- **Long Request Support**: Default 4-hour timeout for slow LLM generation
- **CLI-First Configuration**: No config files needed, everything via command-line arguments
- **Bash Expansion Support**: Space-separated backends enable shell expansion

## Install

Prebuilt binaries for Linux and macOS (amd64/arm64) are on the
[releases page](https://github.com/se-ok/go-load-balance/releases):

```bash
# Example: v0.2.1 on Linux amd64 — installs to ~/.local/bin (make sure it is on your $PATH)
mkdir -p ~/.local/bin
curl -fsSL https://github.com/se-ok/go-load-balance/releases/download/v0.2.1/lb_0.2.1_linux_amd64.tar.gz | tar -xz -C ~/.local/bin lb
lb --help
```

## Build from Source

Requires Go 1.25+. If you don't have Go installed:

```bash
# Install Go without sudo (Linux amd64)
# See https://go.dev/dl/ for other platforms and latest versions
export GO_VERSION=1.25.7
curl -fsSL https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz | tar -C ~/.local -xz
export PATH=$HOME/.local/go/bin:$HOME/go/bin:$PATH
```

Install the load balancer:

```bash
go install ./cmd/lb
```

## Usage

### Basic Usage

```bash
lb --backends http://localhost:8000 http://localhost:8001 http://localhost:8002
```

### Using Bash Expansion

```bash
lb --backends http://localhost:800{0..2}
```

### Full Configuration

```bash
lb \
  --backends http://localhost:8000 http://localhost:8001 http://localhost:8002 \
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
| `--health-check-interval` | Health check interval (minimum `5s`) | `30s` |
| `--routing` | Routing mode: `least-conn` or `cache-aware` | `least-conn` |
| `--max-conns` | Hard limit on concurrent requests per backend, `0` = unlimited (required for `cache-aware`) | `0` |
| `--affinity-ttl` | Cache-aware: sliding lifetime of prefix-affinity entries | `1h` |
| `--log-to` | Append each request/response pair as one JSON object per line (JSONL) to this file | off |
| `--verbose` | Enable verbose logging with per-backend details | `false` |

## How It Works

1. **Load Balancing**: Each request goes to the healthy backend with the fewest active connections (ties broken randomly); the count is updated at selection time, so concurrent bursts spread evenly
2. **Health Checks**: Every 30 seconds (configurable), the load balancer checks all backends' `/v1/models` endpoints concurrently
3. **Fail Fast, Recover Slow**: A backend is marked unhealthy on the first failed health check, proxy error, or proxied 5xx response; 4xx responses (including 429) are passed through without affecting health. An unhealthy backend rejoins the pool after 2 consecutive successful health checks. Health transitions are logged exactly once
4. **Status Logging**: Every 30 seconds, logs total active connections, healthy backend count, and each healthy backend's connection count sorted in decreasing order (with more than 30 backends, only the first and last 15 are shown):
   ```
   [STATUS] Active: 12 | Healthy: 3/3 | Conns/node: [5, 4, 3]
   ```
5. **Transparent Proxying**: Uses Go's `httputil.ReverseProxy` to stream requests/responses without buffering
6. **No Healthy Backends**: When all backends are down, proxied requests return 503 Service Unavailable; when all healthy backends are at `--max-conns`, requests return a provider-style 429 rate-limit error instead (backpressure, not an outage)

## Cache-Aware Routing

`--routing cache-aware --max-conns <n>` routes requests that share a prefix (the same
conversation growing turn by turn, sessions starting from an identical harness prompt,
same-document queries) to the same backend, so a backend's prefix KV cache actually
gets reused instead of being re-prefilled across the pool. Designed for fronting
multiple LLM nodes that each run their own rank-level router.

How it works, briefly:

- The request body is canonicalized (per chat message: role, reasoning /
  reasoning_content, content, tool_calls — a turn with only tool calls still counts)
  and hashed into a cumulative **chain**: one hash per turn, turns over 8k chars split
  into 8k blocks, frozen at 500 units. Equal chain hash ⟺ byte-identical prefix.
- A sticky table maps chain hashes → backend. A request follows its **longest matching
  prefix**; unknown prefixes are placed by least-connections; every request re-points
  its hashes to the backend that actually served it.
- Affinity never overrides load for long: a pinned backend at `--max-conns`, or ahead
  of the least-loaded one by more than 20% of `--max-conns`, overflows to
  least-connections (hot prefixes replicate onto other backends naturally).
- Entries expire after `--affinity-ttl` idle (matching backend KV eviction), the table
  retains at most 5×`--max-conns` recent requests per backend, and a backend that goes
  unhealthy invalidates all of its entries at once.
- Keys are derived from request content only — no custom headers required. The
  standard OpenAI `user` field, when present, overrides content-derived keys.

The `[STATUS]` line gains routing counters:
`... | Affinity: warm 82% cold 15% ovfl 3% (120 reqs, 5731 keys)`.

In both routing modes, `--max-conns > 0` is a hard admission limit: when every healthy
backend is at the cap, requests are rejected immediately — never queued — with an
OpenAI-style 429 (`rate_limit_error`, `Retry-After: 1`).

### Two-Tier Deployment

`lb` can be stacked: one instance per node routing between GPU ranks, one cluster
instance routing between the node instances:

```bash
# node level (per node, fronting 8 vLLM data-parallel ranks)
lb --backends 127.0.0.1:3003{0..7} --routing cache-aware \
   --max-conns 5 --health-check-interval 5s --port 30040

# cluster level (fronting the node instances)
lb --backends node{01..60}:30040 --routing cache-aware --max-conns 40
```

Rules of thumb:

- **Align the caps**: cluster `--max-conns` = node `--max-conns` × ranks per node
  (5 × 8 = 40 above), so the cluster never admits more than a node can hold. At-capacity
  429s from a node pass through the cluster tier as 4xx without affecting its health.
- **A single rank failure ejects the whole node — by design.** A rank's 5xx propagates
  through the node instance to the cluster instance, which marks the node unhealthy.
  Under expert parallelism the ranks share collectives, so one broken rank compromises
  all of them; stopping traffic to the node is the correct response.
- Use a short node-level `--health-check-interval` (the 5s minimum): the cluster's
  probe through a node instance only proves one rank is alive, so fast rank-level
  probing at the node tier is what actually detects partial failures.

## Request/Response Logging

`--log-to <path>` appends every request handled by the pool to a JSON Lines file,
one object per completed request pairing the request body with the response body:

```json
{"time":"2026-07-16T10:34:10.92Z","duration_ms":1523,"method":"POST","path":"/v1/chat/completions","status":200,"backend":"http://127.0.0.1:8000","request":{"model":"m","messages":[...]},"response":"data: {...}\n\ndata: [DONE]\n\n"}
```

- `request`/`response` hold the raw body when it is valid JSON, the body as a
  string otherwise (e.g. SSE streams), or `null` when empty.
- Bodies are tee'd as they stream — proxying stays unbuffered and SSE flushing is
  unaffected; the line is written when the response completes.
- Selection failures are logged too (429/503 with no `backend`); filter with e.g.
  `jq 'select(.status == 200)'`. The `/health` endpoint is not logged.
- The file is opened in append mode, created with permissions `0640` (logged
  conversations are sensitive; pre-create the file if you need different
  permissions). Capture is capped at 1 GiB per body as a DoS guard; a cut-off
  body is flagged `request_truncated`/`response_truncated`.

## Architecture

```
┌─────────────┐
│   Client    │
└──────┬──────┘
       │
       ▼
┌──────────────────────────────────────┐
│   Go Load Balancer                   │
│   - Least-Connections Selector       │
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
- **No request/response buffering**: Request and response bodies are sent directly between client and backend, keeping memory usage minimal regardless of payload size.

## Limitations

- **HTTP only**: No HTTPS/TLS termination. Place behind a TLS-terminating reverse proxy (e.g., nginx, caddy) if you need HTTPS on the frontend. Backends using `https://` URLs work without any changes.
- **Static backend list**: Backends are configured at startup and cannot be changed at runtime. Restart the load balancer to update the backend list.

## License

MIT
