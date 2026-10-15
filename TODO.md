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
- [ ] **2. REST/JSON API** — issues, projects, workflows; CRUD + search
- [ ] **3. Claim API + lease manager** — atomic claim/lease/heartbeat
      (`SELECT … FOR UPDATE SKIP LOCKED`)
- [ ] **4. Workflow engine + eval hooks** — versioned statecharts; eval hooks
      gate terminal transitions
- [ ] **5. MCP server** — ~12 typed tools, thin wrapper over the service layer
      (this is the primary interface)
- [ ] **6. Event bus + SSE** — state-change stream with cursor replay
- [ ] **7. Artifact storage** — typed, content-addressed
      (plan / test_report / diff / log)
- [ ] **8. Minimal web UI** — read-mostly human view (secondary)

Full rationale in `docs/rea-jira.md`.
