# CLAUDE.md

HTTP load balancer for aggregating LLM (OpenAI-compatible) endpoints. Long-lived
streaming requests are the norm — the default request timeout is 4 hours.

## Structure

- `cmd/lb/` — main binary: CLI flags (urfave/cli/v3), HTTP server, `/health` endpoint, graceful shutdown
- `cmd/mock-backend/` — test backend with modes: healthy, slow, failing, flaky, timeout
- `lib/backend.go` — `Backend`: reverse proxy wrapper, health state, active-connection count
- `lib/balancer.go` — `Pool`: backend collection, selection, `ServeHTTP`
- `lib/healthcheck.go` — periodic active health probing
- `lib/logger.go` — periodic `[STATUS]` summary logging
- `test.py`, `test_stress.py` — Python integration tests (no Go tests); `.goreleaser.yaml` for releases

## Design decisions

- **Selection**: "random two-least" — pick 2 random healthy backends, route to the one
  with fewer active connections. Cheap and avoids herding on one idle backend.
- **Health = active probes + passive signals.** The checker GETs `/v1/models` every
  interval (default 30s, hardcoded 5s probe timeout); the proxy also marks a backend
  unhealthy on transport errors and proxied **5xx** responses.
- **4xx (including 429) never affect health.** They are the client's or rate limiter's
  business; ejecting a 429-ing backend shifts load and can cascade 429s across the pool.
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
- Backends start healthy; the status logger delays its first line so the initial
  health sweep can complete first.

## Known gaps (deliberate, not yet addressed)

- Shutdown grace is 10s — LLM streams still running at shutdown are dropped.
- A backend failing mid-stream (after response headers) aborts the client connection
  but is not marked unhealthy: Go's `ReverseProxy` never calls `ErrorHandler` there.

## Conventions

- Go 1.25, single external dependency (`urfave/cli/v3`). Verify with
  `go build ./... && go vet ./...`; exercise behavior via `cmd/mock-backend`.
- Use `any`, not `interface{}`.

## Releasing

Releases are published to GitHub with goreleaser, run locally (no CI). From a
clean, up-to-date `main`:

```bash
git tag -a vX.Y.Z -m "vX.Y.Z" && git push origin vX.Y.Z
GITHUB_TOKEN=$(gh auth token) goreleaser release --clean
```

`.goreleaser.yaml` builds the `lb` binary for linux/darwin × amd64/arm64 and
uploads tar.gz archives plus checksums; the changelog is generated from
commits since the previous tag. When bumping the version, update the install
example in `README.md` to the new tag.
