# TODO — forge-tasks build plan

Stack: **Go** (chosen 2026-10-15 for massive concurrent AI-agent load;
single static binary, goroutine-per-connection concurrency, pgx for Postgres).
Go 1.27.2 toolchain lives at `~/go` (persistent across VM refreshes).

All 8 components built, tested, and pushed. Remaining work is polish, not
structure.

- [x] **1. Issue store** — `store/` package: WorkItem / AcceptanceCriterion /
      AgentPrincipal / Project domain types, Store interface (context-aware,
      optimistic concurrency via Version), concurrency-safe MemoryStore
      reference implementation, Postgres schema in
      `store/migrations/001_init.sql` (JSONB + GIN indexes, key_counters for
      atomic FT-N allocation). Race tests: 32-writer version conflicts,
      100-goroutine key allocation.
- [x] **Postgres adapter** — `store/postgres.go` (pgx/v5 + pgxpool): same
      contract as MemoryStore, row-locked key allocation, optimistic updates,
      `store/migrate.go` embeds the schema and `ApplyMigration` runs it on
      boot. `TestPostgres*` (isolated DB per run) runs in CI against a
      Postgres service.
- [x] **2. REST/JSON API** — `api/` package (stdlib `net/http` only): projects,
      work items (CRUD + sparse PATCH with required version → 409 on stale),
      filtered/paginated listing, agent upsert/get/heartbeat, `/healthz`;
      JSON error envelope, request IDs, structured logging, 1 MiB body cap.
      `cmd/forge-tasks-api`: `DATABASE_URL` selects Postgres (schema
      auto-applied) else in-memory; graceful shutdown. `Dockerfile`
      (multi-stage, static binary) + `api` service in `docker-compose.yml`.
- [x] **4. Claim API + lease manager** — `store.Claim` (atomic pick-one:
      `SELECT … FOR UPDATE SKIP LOCKED` on Postgres, mutex + deterministic
      lowest-key pick on memory), lease TTL (default 5m, max 24h) with
      heartbeat renew, release, lazy expiry of dead leases (no reaper).
      Endpoints: `POST /claims` (201 + item / 204 when empty),
      `GET/DELETE /claims/{key}`, `POST /claims/{key}/renew`,
      `GET /agents/{id}/claims`; ownership enforced (409 `claim_conflict`).
      Migration `002_claims.sql`. 32-agent memory race + 16-goroutine
      Postgres race: exactly 1 winner.
- [x] **5. Workflow engine + eval hooks** — `workflow/` pure library:
      versioned statechart definitions (validated JSON), pure-function
      transition evaluation with declared preconditions (`require_criteria_met`),
      machine-readable "why blocked" answers (`unknown_transition` /
      `criteria_not_met` + `missing` / `available`). Eval hooks run shell
      checks with timeouts (process-group kill), bounded output, env context;
      failures block terminal transitions. `service/` is the single choke
      point: `PATCH` status changes and `POST /transitions` both go through
      it (stale versions 409 before evaluation). Migration `003_workflows.sql`
      (immutable versions, per-project pinning, `eval_runs` audit table);
      new projects auto-pin the default workflow.
- [x] **3. MCP server** — `mcp/` over the service layer (never over HTTP),
      17 typed tools with strict JSON schemas (official Go SDK): projects,
      work items, claims, transitions (with `dry_run`; refusals return
      `allowed: false` with reasons, never bare errors), agents, eval history,
      artifacts. `cmd/forge-tasks-mcp` serves stdio; verified with a real
      JSON-RPC handshake. In-memory client/server tests.
- [x] **6. Event bus + SSE** — `bus/`: non-blocking pub/sub (slow subscribers
      are disconnected and reconnect with cursors), 1024-event replay ring,
      `ErrCursorTooOld` forces explicit resync. Store publishes entity events,
      service publishes `transition.applied`. `GET /api/v1/events` streams
      `id:/event:/data:` frames with 25s pings, `?cursor=` / `Last-Event-ID`
      replay; no `WriteTimeout` (long-lived by design).
- [x] **7. Artifact storage** — content-addressed (SHA-256 dedupe), typed
      (`plan`, `test_report`, `diff`, `log`, `screenshot`, `eval_result`;
      per-kind metadata schemas), versioned per (work item, name).
      Migration `004_artifacts.sql`. Eval-hook outputs are auto-mirrored as
      `eval_result` artifacts (best-effort). REST (`POST/GET /artifacts`,
      `/content` with `ETag`) + MCP tools (`add_artifact`, `get_artifact`,
      `list_artifacts`).
- [x] **8. Minimal web UI** — `ui/`: server-rendered (no JS) dashboard with
      project/status filters, item detail with criteria, one-click governed
      transitions (blocked reasons inline), claim state, eval history,
      artifacts. Mounted at `/` alongside the JSON API.

Full rationale in `docs/rea-jira.md`.

## Open / polish

- [ ] Streamable HTTP transport for the MCP server (stdio works; remote
      agents need HTTP).
- [ ] `watch_events` MCP tool over the bus (REST SSE covers it today).
- [ ] AuthN/Z: everything is currently open (single-trust-domain MVP).
- [ ] Artifact blobs → S3-compatible backend (Postgres BYTEA today).
- [ ] Eval hooks: LLM-judge variant (shell/test commands today).
- [ ] User smoke-test of `docker compose up -d` on a Mac (no Docker on the
      build VM; compose syntax + migration idempotency validated only).
