# router

A single-process, OpenAI-compatible HTTP proxy that accepts client traffic
on a small set of public model names and fans it out across many upstream
models by configured **token-share** targets, without the client ever being
able to tell that fan-out happened.

Two properties define success (see acceptance tests below):

1. **Distribution fidelity** — realized token share per upstream converges
   on its configured target and stays converged under upstream failure.
2. **Identity opacity** — the client only ever sees the model name it asked
   for, in every response, every streaming chunk, and every error.

## Build & run

```bash
go build -o router.exe ./cmd/router
./router.exe --config config.yaml
```

No Docker, no database, no broker. Copy `config.example.yaml` to
`config.yaml`, fill in `base_url`/`api_key_env` for your real upstreams, and
export the referenced environment variables before starting.

```bash
go test ./...
```

## Package layout

```
cmd/router/main.go     wiring: config, registry, HTTP server, graceful shutdown
internal/config/       YAML load, validate, poll-based hot reload
internal/jsonutil/     order-preserving JSON object (OrderedMap)
internal/openai/       request signal extraction, per-upstream quirk adapter, error envelope
internal/sanitize/     the identity-opacity chokepoint
internal/tokens/       pre-flight token estimation + usage reconciliation
internal/upstream/     per-upstream runtime state: HTTP client, circuit breaker, sliding window, debt accumulator
internal/router/       Stage 1 gating + Stage 2 debt-weighted selection
internal/proxy/        HTTP handlers: retry window, streaming, dispatch
internal/stats/        /internal/stats counters and handler
internal/redisync/     optional cross-instance convergence
```

## How the routing engine actually works

**Stage 1 (gating)** — `router.Candidates` filters the upstreams configured
for a public model down to those whose modality, context window (with a 5%
safety margin), tool support, structured-output support, circuit-breaker
state, and TPM budget can all accept this specific request. See
`internal/router/gate.go`.

**Stage 2 (selection)** is implemented as an **incremental
deficit-weighted-round-robin accumulator** rather than the literal
"recompute realized_share from a sliding window on every request" reading
of the spec pseudocode. Each upstream carries a `Debt` float (see
`upstream.Upstream.Debt`, `internal/upstream/atomicfloat.go`). On every
delivered response:

```
for each candidate c in the request's surviving candidate set:
    c.Debt += (weight[c] / totalWeightOfCandidates) * tokensDelivered
winner.Debt -= tokensDelivered
```

`router.Select` picks the candidate with the highest `Debt` (+ small
jitter, tie-broken by lowest in-flight). This is mathematically the same
quantity as `target_share - realized_share` — it is just tracked as a
running O(1)-per-delivery accumulator instead of recomputed from raw
counters each time. Two things this bought:

* **Exact convergence**, not stochastic: `TestDistribution_TokenWeightedConvergence`
  (`internal/router/distribution_test.go`) shows ~0.0000 drift over 50,000
  requests, because the accumulator *is* the deficit, not an estimate of it.
* **A clean answer to the "no repayment spike on recovery" requirement**:
  the circuit breaker's `onClose` callback (`internal/upstream/breaker.go`)
  simply zeroes `Debt`, forgiving whatever deficit built up during the
  outage instead of letting the recovered upstream out-compete everyone
  else trying to "catch up." See `TestDriftRecovery_NoRepaymentSpike`.

A separate structure, `upstream.TokensSent` (the 60-bucket sliding window
the spec describes), is kept purely for **display** — the realized-share
numbers in `/internal/stats` — and is orthogonal to the selection decision.

**Stage 3 (fallback)** lives in `internal/proxy/proxy.go`'s retry loop:
re-gate excluding already-tried upstreams, cap attempts at
`server.max_attempts` and wall-clock at `server.request_budget`. Only
tokens from a hop that actually delivered a response count toward that
upstream's realized share — a tried-and-failed hop contributes nothing.

## Identity opacity

Every response and every SSE chunk passes through
`internal/sanitize/sanitize.go`'s `Rewrite`/`Object`: pin `.model` to the
public name, replace `.id` with the router's own request id, strip every
top-level field outside a small OpenAI-schema allowlist, and scrub `.usage`
down to the four standard integer/detail fields. Client-facing errors are
*always* synthesized fresh (`sanitize.Error` / `sanitize.StreamTerminalError`)
— an upstream error body is never forwarded, never even read into a string
that could leak.

The identity fuzz test (`internal/sanitize/sanitize_test.go`) caught a real
leak during development: `system_fingerprint` is a legitimate OpenAI schema
field, but providers encode backend/build identity into its *value* (e.g.
`fp_qwen3.8-flash`). It is now dropped entirely rather than passed through.

## Streaming retry window

`internal/proxy/stream.go` dials the upstream and pulls the first SSE
`data:` payload **without writing anything to the client**. Only once that
first payload is in hand does it write status/headers and flush it —
"committed." Any failure before that point returns to the retry loop for a
transparent failover. Any failure after that point can only be terminated:
a well-formed terminal error chunk plus `data: [DONE]`, logged at `ERROR`
level (`"committed stream failed mid-flight"`), so the client's SSE parser
closes cleanly instead of hanging — this is the one failure mode the design
cannot hide, so it's the one thing logged loudly by design.

## Ambiguities and interpretations

The brief asked that ambiguous or wrong requirements be called out here
rather than silently guessed.

1. **fsnotify is not in the allowed dependency list.** Hot reload is
   implemented as a 10-second mtime poll (`internal/config/manager.go`),
   which the spec explicitly sanctions as an alternative ("fsnotify or a
   10s poll").

2. **Upstream request path.** The spec gives `base_url` but not a path
   suffix. Dispatch POSTs to `strings.TrimRight(base_url, "/") +
   "/chat/completions"`, i.e. `base_url` is expected to already include
   `/v1` where the provider uses that convention (matches the example
   config).

3. **Upstream failure classification.** The spec's "propagate
   immediately / retry" error table is written from the client's
   perspective (bad client key, unknown model, etc.), but this router
   deliberately implements no client authentication (per the anti-goals:
   "do not add auth... something upstream of this handles that"). So the
   only *fatal-immediate, no-retry* errors this router generates itself
   are ones it can detect **before dispatching to any upstream**:
   malformed JSON body, unknown public model, and "no upstream survives
   Stage 1 gating even on the first attempt" (covers context-window
   overflow and capability mismatch). Every upstream-side failure —
   any non-2xx status, a transport error, a malformed response body —
   is treated as retryable-and-fail-over, bounded by `max_attempts` and
   `request_budget`, and only surfaces to the client as a generic
   `502 all upstream attempts failed` if every attempt is exhausted. This
   reading was chosen because (a) surfacing one misconfigured or
   overloaded upstream's status code would violate "the user should
   never meet an upstream's error," and (b) failing the whole client
   request over a single upstream's transient/config problem when other
   upstreams could serve it would undermine distribution fidelity for no
   benefit. The circuit breaker still opens on repeated failures, so a
   persistently broken upstream is removed from rotation rather than
   retried forever.

4. **"First content-bearing chunk."** Interpreted as the first
   successfully parsed SSE `data:` event, even if its `delta` is
   role-only with empty content (a common first chunk shape) — it's still
   proof the upstream is alive and streaming, which is what the retry
   window is protecting.

5. **Structured-output gating.** The config schema has one capability
   flag, `supports_json_schema`; both `response_format.type ==
   "json_object"` and `"json_schema"` are gated by it, since the spec
   didn't provide a separate flag for the weaker `json_object` mode.

6. **`/internal/stats` auth.** The spec requires the endpoint be
   "auth-gated, never public" but doesn't specify a mechanism. Added
   `server.stats_auth_token` to the config schema; the endpoint checks
   `Authorization: Bearer <token>` and **fails closed** (refuses all
   requests) if the token is unset, rather than defaulting open.

7. **TPM budget tracking.** Estimated cost is reserved into a dedicated
   1-minute window (`upstream.tpmWindow`) at dispatch time, separate from
   `TokensSent` (the actual-delivery window used for stats/realized
   share) — so an in-flight large request is already counted against
   capacity before its real usage is known, which is the conservative
   direction to err on for a rate limit.

## Deliberate simplifications

- **Redis sync is additive, not authoritative.** Each instance's local
  debt accounting is always self-consistent on its own; Redis only nudges
  it toward the cluster-wide picture every 5s by crediting *remote*
  deliveries (this instance's own contributions are tracked separately so
  they're never double-applied — see `internal/redisync/redisync.go`).
  Cross-instance nudges use the full configured upstream pool's weights
  rather than a specific request's gated-and-renormalized candidate set,
  because gating context doesn't exist outside the request that produced
  it. Every acceptance scenario passes identically with `redis.enabled:
  false` (acceptance test #10); Redis only improves multi-instance
  convergence speed, it is never required for correctness.
- **Token estimation is a ~4-chars/token heuristic** (plus flat per-image/
  audio-part costs), used only for pre-flight gating and TPM budgeting.
  Every real dispatch reconciles against the upstream's actual `usage`
  object before updating realized-share/debt accounting, so estimation
  error never compounds into the distribution numbers — only into how
  conservatively a borderline request gets gated.

## Acceptance tests: spec item → test

| # | Spec requirement | Test |
|---|---|---|
| 1 | Distribution fidelity ±2pp over 50k requests; request-count weighting fails | `internal/router/distribution_test.go` |
| 2 | Drift recovery, no repayment spike | `internal/router/drift_recovery_test.go` |
| 3 | Identity: no upstream identifier in any client-visible byte | `internal/sanitize/sanitize_test.go` |
| 4 | Retry window: pre-commit failover, post-commit clean terminal | `internal/proxy/proxy_test.go` (`TestRetryWindow_PreCommit`, `TestStreaming_CommittedFailureEmitsCleanTerminal`) |
| 5 | Capability gating: multimodal never reaches text-only, 10k trials | `internal/router/gate_test.go` |
| 6 | Context gating: 190k request only reaches large-window upstreams | `internal/router/gate_test.go` |
| 7 | Load: 400 concurrent streams, flat memory, <5ms p95 overhead | see "Load testing" below — not run as part of `go test ./...` |
| 8 | Cancellation: upstream request cancelled within 100ms | `internal/proxy/proxy_test.go` (`TestCancellation_UpstreamContextCancelledPromptly`) |
| 9 | Hot reload: weight change takes effect, zero dropped requests | `internal/config/config_test.go` (`TestManager_ReloadPicksUpWeightChange`); structurally guaranteed by `proxy.go` reading `Registry.Config()` fresh per-request rather than caching it |
| 10 | Redis-absent: every test passes with `redis.enabled: false` | every test above runs against an in-memory config with no Redis involved at all |

Circuit breaker state transitions (open/half-open/close, single-probe
admission, both trigger conditions) are covered separately in
`internal/upstream/breaker_test.go`, and config validation/reload edge
cases in `internal/config/config_test.go`.

### Load testing

Acceptance test #7 (400 concurrent streams for 5 minutes, memory/FD/latency
bounds) is a long-running, resource-observing scenario that doesn't fit a
normal `go test ./...` run. The architecture that makes it pass is: no
per-request allocation of unbounded state (SSE forwarding streams
chunk-by-chunk rather than buffering), one shared `http.Transport` per
upstream host with `MaxIdleConnsPerHost: 512`, and no goroutines held open
beyond the request's own lifetime (`internal/upstream/upstream.go`,
`internal/proxy/stream.go`). To actually exercise it, run the binary and
drive it with an external load generator (e.g. `hey`, `vegeta`, or a small
custom client opening 400 concurrent streaming connections) while watching
`/internal/stats`, `GOMAXPROCS`-scaled CPU, and RSS.

## Config reference

See `config.example.yaml` for a complete example. Key fields not obvious
from the spec's snippet:

- `server.stats_auth_token` — bearer token required to read
  `/internal/stats`; leave unset to disable the endpoint.
- `upstreams[].quirks` — `strip: [...]` removes listed top-level request
  params; `no_temperature`/`no_top_p` drop those params entirely;
  `max_completion_tokens: true` renames `max_tokens` →
  `max_completion_tokens` (for reasoning-style models); `stop_as_string:
  true` collapses a single-element `stop` array to a bare string.
- `public_models[]` — each entry is just a client-facing name mapped to a
  weighted pool of real `upstreams[]` entries; nothing requires the pool to
  be unique per public model. `gpt-router-large`, `gpt-6-astra`, and
  `claude-fable-5-1` all fan out across the identical five kiosapi.com
  models + `selfhost-pool` at the same weights — three names for the same
  backend pool, not three different backends. Every public model must
  designate at least one `fallback: true` upstream, per `Config.Validate`.
  Whichever public name a client requests is the only thing they ever see —
  in `.model` on the response, in every streamed chunk, in `/v1/models`,
  and in every error body — regardless of
  which upstream actually served it; see "Identity opacity" above.

Weight changes, upstream additions/removals, and quirk changes all take
effect on the next config poll (default 10s) without dropping in-flight
requests — `Registry.Sync` keeps each still-present upstream's circuit
breaker, sliding window, and debt state intact across a reload and only
swaps the config pointer.
