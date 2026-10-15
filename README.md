# forge-tasks

[![CI](https://github.com/azmi2409/forge-tasks/actions/workflows/ci.yml/badge.svg)](https://github.com/azmi2409/forge-tasks/actions/workflows/ci.yml)
[![Go](https://img.shields.io/badge/go-1.27-blue)](https://go.dev)
[![License: MIT](https://img.shields.io/badge/license-MIT-green)](LICENSE)

![forge-tasks: AI agents collaborating on a task board](docs/images/hero.png)

**Jira, rebuilt for AI agents.**

An open-source project management system designed agent-first: work items are
structured tasks with machine-readable acceptance criteria, agents are
first-class principals that claim and lease work, and the primary interface is
an MCP server — not a human UI.

> Status: building. Component 1 (issue store) is done — see below. The
> reverse-engineering analysis of Jira the design is based on is in
> `docs/rea-jira.md`; the full roadmap is in `TODO.md`.

## Why not just use Jira?

The REA found Jira's model fights agents at every step:

- **No agent identity** — an API token is a user, a Forge app is an app user.
  Agents are second-class citizens.
- **Points-based rate limits** punish the traversal loops agents naturally do
  (Global Pool 65k/hr default; per-tenant pools up to a 500k cap).
- **JQL can't subquery, join linked issues, or aggregate** — "JQL simply finds
  issues" — so agents do N+1 loops client-side. Bulk ops cap at 1,000.
- **"Done" is a status, not a checkable predicate** — acceptance criteria live
  in unstructured blobs; nothing verifies them.

## How it works

![An agent's work passes through the eval gate before it counts as done](docs/images/eval-gate.png)

An agent claims a work item, executes it, and the acceptance criteria are
evaluated by machine — tests run, commands exit 0, files exist. Only when every
criterion passes does the item move to done. No faith-based status updates.

```mermaid
flowchart LR
    A[Agent claims work] --> B[Executes task]
    B --> C{Eval hooks}
    C -->|all criteria pass| D[Done]
    C -->|criterion fails| B
```

## Architecture

```mermaid
flowchart TB
    Agent[AI agents] <--> MCP[MCP server]
    Agent <--> API[REST/JSON API]
    MCP --> Store[(Issue store)]
    API --> Store
    Claim[Claim API + lease manager] --> Store
    WF[Workflow engine + eval hooks] --> Store
    Store --> Events[Event bus + SSE]
    Store --> Artifacts[(Artifact storage)]
    UI[Minimal web UI] --> API
```

Built for massive concurrent agent load: optimistic concurrency on every write,
atomic per-project key allocation, context-aware store interface backed by a
Postgres connection pool in production.

## Quickstart

```bash
# Run everything (Postgres 16 + API)
docker compose up -d

# Or run the API locally against the store of your choice:
go run ./cmd/forge-tasks-api                      # in-memory store
DATABASE_URL=postgres://forge:forge@localhost:5432/forge_tasks?sslmode=disable \
  go run ./cmd/forge-tasks-api                    # Postgres (schema auto-applied)

# Tests (Go 1.27+)
go test ./... -race
```

## API

Base URL: `http://localhost:8080/api/v1` (`PORT` env overrides the port).

| Method | Path | Description |
| ------ | ---- | ----------- |
| GET | `/healthz` | Liveness probe |
| POST | `/projects` | Create a project |
| GET | `/projects/{key}` | Get a project |
| POST | `/projects/{key}/next-key` | Atomically allocate the next work-item key |
| POST | `/work-items` | Create a work item (key auto-allocated) → `201` |
| GET | `/work-items` | List with `?project=&status=&assignee=&issue_type=&labels=a,b&limit=&offset=` |
| GET | `/work-items/{key}` | Get one |
| PATCH | `/work-items/{key}` | Sparse update; `version` required → `409` on stale write |
| DELETE | `/work-items/{key}` | Delete (key never reused) → `204` |
| PUT | `/agents/{id}` | Create/replace an agent principal |
| GET | `/agents/{id}` | Get an agent principal |
| POST | `/agents/{id}/heartbeat` | Record liveness (`{"at": "..."}` optional) |

Errors are `{"error": {"code": "...", "message": "..."}}` with codes
`not_found` (404), `already_exists` / `version_conflict` (409),
`validation_error` / `bad_request` (400).

```bash
curl -X POST localhost:8080/api/v1/projects \
  -d '{"key":"FT","name":"Forge Tasks"}'
curl -X POST localhost:8080/api/v1/work-items \
  -d '{"project_key":"FT","title":"Ship it","labels":["backend"]}'
# → {"key":"FT-1", ... "version":1}
curl -X PATCH localhost:8080/api/v1/work-items/FT-1 \
  -d '{"version":1,"status":"doing"}'
```

## Deploy

```bash
docker compose up -d   # Postgres 16 + API, schema auto-applied
```

See [docs/DEPLOY.md](docs/DEPLOY.md).

## Layout

- `store/` — component 1: the issue store. Domain types (`WorkItem`,
  `AcceptanceCriterion`, `AgentPrincipal`, `Project`), the `Store` interface,
  a concurrency-safe in-memory reference implementation, the pgx Postgres
  adapter, and the schema in `store/migrations/001_init.sql`.
- `api/` — component 2: the REST/JSON API (stdlib only).
- `cmd/forge-tasks-api/` — the API server binary.
- `docs/rea-jira.md` — the Jira reverse-engineering analysis.
- `docs/images/` — README artwork.

## The design

- Structured acceptance criteria as checkable predicates
- Agent principals with atomic claim/lease/heartbeat (CI-runner semantics)
- MCP server (~12 typed tools) as the primary interface
- API-driven, versioned workflow statecharts with machine-readable "why blocked"
- A small predicate query language with first-class relations (no JQL gaps)
- SSE event streaming with cursor replay
- Typed, content-addressed artifacts (plan, test_report, diff, log)
- Sub-agent handoffs with optimistic concurrency
- Eval hooks gating terminal transitions
- Minimal read-mostly human UI (secondary)

## Build plan

Eight components, in build order (see `TODO.md`):

1. Issue store ✅ (incl. Postgres adapter)
2. REST/JSON API ✅
3. Claim API + lease manager
4. Workflow engine + eval hooks
5. MCP server
6. Event bus + SSE
7. Artifact storage
8. Minimal web UI

Closest open-source bases if we ever fork instead of building ground-up:
Plane (AGPL) or Taiga (MPL). Best API-shape inspiration: Linear's sparse updates
plus YouTrack's field projection.

## License

MIT — see `LICENSE`.
