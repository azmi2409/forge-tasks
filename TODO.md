# TODO — forge-tasks build plan

Components in build order (component numbers kept stable):

- [ ] **1. Issue store** — Postgres + JSONB; work items, structured acceptance
      criteria, agent principals
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
