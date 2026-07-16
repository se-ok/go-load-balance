# CLAUDE.md

HTTP load balancer for aggregating LLM (OpenAI-compatible) endpoints. Long-lived
streaming requests are the norm — the default request timeout is 4 hours.

## Structure

- `cmd/lb/` — main binary: CLI flags (urfave/cli/v3), HTTP server, `/health` endpoint, graceful shutdown
- `cmd/mock-backend/` — test backend with modes: healthy, slow, failing, flaky, timeout
- `lib/backend.go` — `Backend`: reverse proxy wrapper, health state, active-connection count
- `lib/balancer.go` — `Pool`: backend collection, selection, `ServeHTTP`
- `lib/cacheaware.go` — `--routing cache-aware`: prefix-affinity routing (chain hashing, sticky table, load guard)
- `lib/reqlog.go` — `--log-to`: JSONL request/response pair logging (tee'd capture, never buffers the proxy path)
- `lib/healthcheck.go` — periodic active health probing
- `lib/logger.go` — periodic `[STATUS]` summary logging
- `test.py`, `test_stress.py` — Python integration tests (no Go tests); `.goreleaser.yaml` for releases

## Design decisions

- **Selection**: least-connections with random tie-break. The connection slot is
  reserved (incremented) inside `SelectBackend` under the pool write lock, so
  concurrent selections see each other's picks and even a simultaneous burst
  spreads within ±1. Power-of-two sampling was dropped deliberately: it hedges
  against stale load info in distributed balancers, but this LB is one process
  with live counters, so full least-conn is strictly better balanced (~3× lower
  skew in simulation). Revisit only if multiple lb instances ever share a pool.
- **Health = active probes + passive signals.** The checker GETs `/v1/models` every
  interval (default 30s, minimum 5s — enforced in `cmd/lb`); the proxy also marks a
  backend unhealthy on transport errors and proxied **5xx** responses. The probe
  timeout is `min(10s, max(4.5s, interval − 0.5s))`: tracking the interval keeps
  short-interval sweeps on cadence, the 10s cap keeps hang detection fast at long
  intervals. The integration tests run at the 5s minimum; recovery waits there must
  cover two sweeps (hysteresis).
- **4xx (including 429) never affect health.** They are the client's or rate limiter's
  business; ejecting a 429-ing backend shifts load and can cascade 429s across the pool.
  This extends to active probes: a 429 answer to the health check counts as a *passing*
  probe (saturated ≠ down — in two-tier deployments a full node lb answers probes with
  429), while any other non-2xx probe status marks the backend unhealthy.
- **Fail fast, recover slow.** One failure marks a backend unhealthy immediately, but
  recovery requires `healthyThreshold` (2) consecutive passing health checks. This is
  hysteresis against flapping: an LLM server whose `/v1/models` responds while real
  inference fails would otherwise rejoin the pool every interval.
- **Log health transitions exactly once.** All state changes go through
  `Backend.MarkUnhealthy()` / `RecordCheckSuccess()`, which return whether a transition
  happened; callers only log when true. Never log per failed request — with many
  concurrent requests to a bad backend that floods the log.
- **Client cancellations are not backend failures.** The proxy `ErrorHandler` checks
  `r.Context().Err()` and skips marking unhealthy when the client disconnected.
- **Health probes run concurrently** (one goroutine per backend per sweep). Sequential
  probing let a few timing-out backends push a sweep past the check interval.
- **Idle connections to backends are discarded after 3s** (`backendIdleConnTimeout`,
  shared by the proxies and the health checker). vLLM's OpenAI server hardcodes
  uvicorn's server-side keep-alive at 5s; reusing a connection the server is
  concurrently closing causes spurious EOF/reset proxy errors that eject healthy
  backends. The client side of a hop must always time out idle connections before
  the server side does.
- Backends start healthy; the status logger delays its first line so the initial
  health sweep can complete first.
- **Cache-aware routing = chunked-turn chained hashing, not a radix trie.** The
  request's message stream is canonicalized (role + reasoning/reasoning_content +
  content + tool_calls per turn; turns > 8k chars split into 8k blocks; frozen at 500
  units) and hashed into a cumulative chain — equal hash proves the whole prefix
  matches, so the routing state is a flat `hash → backend` map with no text retention
  and no structural invariants (eviction can degrade routing but never corrupt it).
  Deepest table hit wins; cold keys place by least-conn; **pin-follows-reality** (all
  hashes re-point to whichever backend actually served the request, so hot shared
  prefixes replicate under load instead of thrashing one node). A trie would only add
  sub-block precision — deliberately rejected; the per-node router handles fine-grained
  matching one tier below.
- **Affinity must expire like the KV cache it mirrors.** Entries have a sliding TTL
  (`--affinity-ttl`, default 1h) and per-backend retention of 5×`--max-conns` recent
  requests; a healthy→unhealthy transition bumps `Backend.epoch`, instantly
  invalidating all pins to it (a relaunched endpoint on the same URL inherits nothing).
- **The load guard is scaled by `--max-conns`** (required for cache-aware): overflow
  to least-conn when the pinned backend is at the cap or leads the least-loaded one by
  > 0.2×cap. In both modes `--max-conns` is a hard admission limit, never a queue.
- **At-capacity is 429, outage is 503.** All healthy backends at the cap → OpenAI-style
  429 `rate_limit_error` with `Retry-After: 1`; zero healthy backends → 503. The
  distinction is load-bearing for two-tier (node lb + cluster lb) deployments: 429 is
  4xx, so a saturated node is *not* ejected by the cluster tier, while a node whose
  ranks are all down 503s and is. Do not collapse these into one status.
- **`--log-to` captures by tee, never by buffering.** Request bodies are tee'd on the
  way to the backend and response bytes on the way to the client, so streaming (SSE
  flushing via `ResponseController` → the wrapper's `Unwrap`) is untouched; the JSONL
  line is written when the response completes. Capture is capped at 1 GiB per body
  (DoS guard only, mirrors `affinityMaxBody`) with `*_truncated` flags. Selection
  failures (429/503) are logged with no `backend`; `/health` is never logged. The
  capture buffer is mutex-guarded because the transport's write loop can still be
  draining the request body when the pair is finalized.
- **Two-tier stacking is supported and documented in the README**: align caps as
  cluster `--max-conns` = node `--max-conns` × ranks. A single rank 5xx propagating up
  and ejecting the whole node at the cluster tier is intended (expert parallelism
  couples the ranks); the weak cluster-through-node health probe (proves only one rank
  alive) is accepted and compensated by a short node-level check interval.

## Known gaps (deliberate, not yet addressed)

- Shutdown grace is 10s — LLM streams still running at shutdown are dropped.
- A backend failing mid-stream (after response headers) aborts the client connection
  but is not marked unhealthy: Go's `ReverseProxy` never calls `ErrorHandler` there.

## Conventions

- Go 1.25, single external dependency (`urfave/cli/v3`). Verify with
  `go build ./... && go vet ./... && go test ./...`; exercise end-to-end behavior via
  `cmd/mock-backend` and the Python suite (`test.py`).
- Go unit tests live next to the code (`lib/*_test.go`) and cover pure logic (chain
  derivation, table TTL/retention, selection); routing behavior over real HTTP is
  covered in `test.py`. `cmd/mock-backend` simulates a prefix KV cache and exposes
  `/prefixstats` so tests can compare reuse ratios between routing modes.
- Use `any`, not `interface{}`.

## Releasing

Releases are published to GitHub with goreleaser, run locally.

**Never push to `main` directly — even when admin rights would bypass the
protection rule.** `main` requires a PR so the CI status checks (build/test,
gosec, govulncheck) gate every release. The flow:

1. Commit the release changes (including the README install-example bump) to a
   feature branch and push that branch.
2. Let the user create the PR, wait for CI to finish its status checks, and
   merge manually.
3. Only then, from the clean, up-to-date merged `main`:

```bash
git tag -a vX.Y.Z -m "vX.Y.Z" && git push origin vX.Y.Z
GITHUB_TOKEN=$(gh auth token) goreleaser release --clean
```

`.goreleaser.yaml` builds the `lb` binary for linux/darwin × amd64/arm64 and
uploads tar.gz archives plus checksums; the changelog is generated from
commits since the previous tag. When bumping the version, update the install
example in `README.md` to the new tag.

The version string reported by `lb --version` (and the `VERSION:` section of
the help text) is never edited by hand: `var version = "dev"` in
`cmd/lb/main.go` is stamped at build time by the
`-X main.version={{ .Version }}` ldflag in `.goreleaser.yaml`, which takes the
version from the git tag being released. Plain `go build` binaries therefore
report `dev` — only goreleaser-built artifacts carry a real version. To verify
locally without tagging: `goreleaser build --snapshot --clean --single-target`,
then run the binary under `dist/` with `--version`.
