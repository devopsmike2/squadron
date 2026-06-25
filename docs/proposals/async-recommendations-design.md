# Async discovery recommendations — design (Fix #3)

## Problem

`POST /discovery/{cloud}/connections/:id/recommendations` calls the AI
proposer (Anthropic, `claude-sonnet-4-6`, `max_tokens=8192`) **inline** and
holds the HTTP request open until it returns. For discovery-sized plans the
model takes 30s–120s+ (measured: a direct probe ran >2 min). The request
times out (`Client.Timeout exceeded while awaiting headers`) and the operator
sees a failure even though the model is working. Confirmed in the real-AWS
e2e; the synchronous design is the root cause, not any single timeout value.

## Decision: job-based kick-off + poll

`POST` becomes a **kick-off**: it does the fast validation synchronously,
starts the proposer in a background goroutine, and returns **202 Accepted**
with a `job_id`. The client **polls** `GET …/recommendations/jobs/:jobID`
until the job is `succeeded` (carries the recommendations payload) or
`failed` (carries the humanized error).

### Why polling over SSE / long-poll

| Option | Verdict |
|--------|---------|
| **Explicit poll (chosen)** | Robust behind any reverse proxy; no long-held connections; trivially testable; works with any client. Standard REST async. |
| SSE / streaming | Nicer "live" feel but adds proxy-buffering failure modes, a long-held connection (the exact thing we're removing), and harder tests. The model gives no real progress signal to stream anyway. |
| Long-poll | Just a fragile middle ground — still holds the connection. |

### UX: honest "pending" state, not a fake progress bar

The model exposes no progress percentage, so a progress bar would be
invented. The UI shows a **spinner + "Generating recommendations… this can
take up to ~2 minutes"** and **auto-polls** (~2s interval) — no manual
refresh button, no fabricated progress. When the job lands it swaps to the
Recommendations view. On failure it shows the humanized error with retry.
(This is the more honest of the two options the brief flagged, so it ships
without a check-in.)

## Substrate

`recommendationJobStore` (in `internal/api/handlers/recommendation_jobs.go`):

- In-memory `map[string]*RecommendationJob` guarded by a mutex.
- `RecommendationJob{ ID, Provider, AccountID, Status, ResultJSON, Err*, CreatedAt, UpdatedAt }`.
  `Status ∈ {pending, running, succeeded, failed}`.
- `Create()` → new pending job + id (uuid). `Run(job, fn)` executes `fn` in a
  goroutine with a **detached** `context.Background()` + 5-min cap (so the
  request returning does not cancel the proposer). `Get(id)` for the poll.
- A reaper drops jobs older than a TTL (default 1h) so the map can't grow
  unbounded.

### Trade-off (documented honestly)

Jobs are **per-process and in-memory**. On a server restart mid-job, or in a
multi-replica deployment without sticky routing, the poll may `404` and the
client re-submits. That is acceptable: the propose call is **idempotent and
read-only** (it writes no cloud state; it only drafts Terraform), and Squadron
OSS runs single-replica by default. Persisting jobs to the app DB is a
straightforward future extension if multi-replica becomes common — called out,
not built, to keep this change scoped.

## Chunks

1. **Substrate + AWS** (this arc): job store + reaper + tests; refactor
   `HandleAWSGenerateRecommendations` so the post-validation work runs in a
   job; add `HandleAWSRecommendationJobStatus` + route; AWS UI auto-poll;
   Go + UI tests. Ship.
2. **Other surfaces**: wire GCP / Azure / OCI recommendations to the same
   store + a shared job-status route; their UIs reuse the AWS poll hook. Ship.

## Backward compatibility

The `POST` response shape changes (200-with-body → 202-with-job_id). The only
consumer is Squadron's own UI, updated in the same chunk. The proposer-bench
calls the proposer in-process (not this endpoint) and is unaffected.
