# forge-tasks

Jira, rebuilt for AI agents.

An open-source project management system designed agent-first: work items are
structured tasks with machine-readable acceptance criteria, agents are
first-class principals that claim and lease work, and the primary interface is
an MCP server — not a human UI.

> Status: early. This repo currently holds the reverse-engineering analysis of
> Jira the design is based on (`docs/rea-jira.md`), plus the build roadmap.
> Code comes next.

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

1. Issue store (Postgres + JSONB)
2. REST/JSON API
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
