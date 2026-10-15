# TODO — forge-tasks build plan

Stack: **Go** (chosen 2026-10-15 for massive concurrent AI-agent load;
single static binary, goroutine-per-connection concurrency, pgx for Postgres).
Go 1.27.2 toolchain lives at `~/go` (persistent across VM refreshes).

Components in build order (component numbers kept stable):

- [x] **1. Issue store** — `store/` package: WorkItem / AcceptanceCriterion /
      AgentPrincipal / Project domain types, Store interface (context-aware,
      optimistic concurrency via Version), concurrency-safe MemoryStore
      reference implementation, Postgres schema in
      `store/migrations/001_init.sql` (JSONB + GIN indexes, key_counters for
      atomic FT-N allocation). 12 tests green with `-race`.
- [x] **Postgres adapter** — `store/postgres.go` (pgx/v5 + pgxpool): same
      contract as MemoryStore, row-locked key allocation, optimistic updates,
      `store/migrate.go` embeds the schema and `ApplyMigration` runs it on
      boot. `TestPostgresStore` (isolated DB per run, skips without
      `TEST_DATABASE_URL`) runs in CI against a Postgres service.
- [x] **2. REST/JSON API** — `api/` package (stdlib `net/http` only): projects,
      work items (CRUD + sparse PATCH with required version → 409 on stale),
      filtered/paginated listing, agent upsert/get/heartbeat, `/healthz`;
      JSON error envelope, request IDs, structured logging, 1 MiB body cap,
      server timeouts. `cmd/forge-tasks-api` binary: `DATABASE_URL` selects
      Postgres (schema auto-applied) else in-memory; graceful shutdown.
      Smoke-tested live (create → patch → stale-409 → filtered list).
      `Dockerfile` (multi-stage, static binary) + `api` service wired into
      `docker-compose.yml`. 6 API handler tests green with `-race`.
- [x] **3. Claim API + lease manager** — `store.Claim` (atomic pick-one:
      `SELECT … FOR UPDATE SKIP LOCKED` on Postgres, mutex + deterministic
      lowest-key pick on memory), lease TTL (default 5m, max 24h) with
      heartbeat renew, release, lazy expiry of dead leases (no reaper
      process needed). Endpoints: `POST /claims` (201 with claimed item /
      204 when the queue is empty), `GET /claims/{key}`,
      `DELETE /claims/{key}?agent_id=`, `POST /claims/{key}/renew`,
      `GET /agents/{id}/claims`; ownership enforced (409 `claim_conflict`).
      Migration `002_claims.sql`. 32-agent memory race + 16-goroutine
      Postgres race: exactly 1 winner, losers get 204.
- [ ] **4. Workflow engine + eval hooks** — versioned statecharts; eval hooks
      gate terminal transitions
- [ ] **5. MCP server** — ~12 typed tools, thin wrapper over the service layer
      (this is the primary interface)
- [ ] **6. Event bus + SSE** — state-change stream with cursor replay
- [ ] **7. Artifact storage** — typed, content-addressed
      (plan / test_report / diff / log)
- [ ] **8. Minimal web UI** — read-mostly human view (secondary)

Full rationale in `docs/rea-jira.md`.
