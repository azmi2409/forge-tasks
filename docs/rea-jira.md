# Reverse-Engineering Analysis: Atlassian Jira + "Jira for Agents" Gap Analysis

Research date: 2026-10-15. Focus: Jira Cloud; Data Center/Server noted where relevant.

**Conventions:** each factual claim carries its provenance: `verified live` (read in the research browser on 2026-10-15), `index` (read via the web-index document fetch on 2026-10-15), or `inference` (my analysis). Secondary skill/agent docs are third-party; where they corroborate Atlassian's own docs I cite the Atlassian doc.

## Summary

**Jira** is a multi-tenant SaaS issue tracker (Cloud) with self-managed cluster (Data Center) and single-server (Server, EOL Feb 2024; Data Center EOL announced 2029) variants. Its core is a highly configurable issue store: every issue is a row plus arbitrary custom fields, driven through project-specific workflows (statuses → transitions with validators/conditions/post-functions), screen schemes (which fields are visible/editable), and permission schemes (who may do what). Search is backed by a text index (Lucene historically; OpenSearch in Data Center; Cloud internals not publicly documented). The REST API v3 is broad (80+ resource families), authenticated via API tokens (Basic) or OAuth 2.0 3LO, and in 2026 moved to a three-layer rate-limit model (points-per-hour quota, per-second burst, per-issue write caps). Extension happens through Forge (Atlassian-hosted functions, the strategic direction) or Connect (remote apps over JWT, being phased out); eventing is via webhooks or Forge triggers.

**Why it hurts AI agents:** everything is configured for human administrators in the web UI. REST behavior is coupled to screen configuration (a field absent from the edit screen cannot be updated via REST); the data model is verbose and self-describing but schema-on-read (custom fields are `customfield_10020`, types in create-metadata don't always match create semantics); rich text is ADF JSON, not markdown; JQL is expressive for humans but lacks subqueries/recursion/aggregation, forcing multi-hop client-side joins that burn rate-limit points; there is no first-class agent identity (an API token is a human user; Forge apps act as app users), no claim/poll semantics, no structured acceptance criteria, and every state change is a comment-plus-transition ritual designed for humans reading a ticket.

**For a "Jira for agents" build:** the open-source precedents closest to agent-first are **Plane** (clean REST, but API quirks and filtering gaps), **YouTrack** (field-projection API + permanent tokens, but proprietary), **Linear** (GraphQL-first sparse updates — the best API-shape precedent, but proprietary), **GitHub Issues** (REST + markdown, simplest mental model, but workflow semantics live in GraphQL-only Projects v2), **OpenProject** (HAL hypermedia API, enterprise-shaped but LLM-unfriendly), and **Taiga** (simple REST, plus a community MCP-server precedent). The recommended design: ~7 components — Postgres issue store, JSON REST API, MCP server as the *primary* agent interface, claim API for work distribution, deterministic workflow engine, event bus with SSE streaming, artifact storage, and a minimal human UI. The core design moves: structured acceptance criteria as first-class schema, agent identities with claim/lease semantics, an LLM-simple query language, plans/artifacts as typed attachments, and eval hooks — none of which exist in Jira today.

---

## PART 1 — Jira as it exists today

### 1. Architecture: Cloud vs Data Center vs Server

**Deployment models** (`index`; community sources corroborate Atlassian announcements):

| | Jira Cloud | Jira Data Center | Jira Server |
|---|---|---|---|
| Hosting | Atlassian-managed SaaS, multi-tenant | Self-managed, single- or multi-node cluster | Self-managed single server |
| DB | Per-tenant provisioning via Monarch ("provides a database for the product instance"); tenancy coordinated through Catalogue Service → Tenant Context Service (TCS) | Cluster shares **one relational database** | Local relational DB |
| Status | Current, all Atlassian R&D investment | **End of life announced for 2029-03-28** (Ascend to Cloud) | **Support ended 2024-02-15** |
| Customization | Limited; apps via Forge/Connect only | P2 plugins (in-process Java), internal APIs, DB access | Same as DC single-node |

**Cloud internals (partially public)** — from Atlassian's SOC3 compliance doc (`index`): attachments live in the Media Platform backed by Amazon S3, **segregated per tenant by a unique identifier** stored in the product database, with a DynamoDB table mapping customer → S3 objects. Database logs retained ≥24h, backups 30 days. Atlassian does not publicly document the Cloud search engine or primary datastore at engineering depth — treat any "Cloud runs Lucene/Elasticsearch" claim as unverified (Could not verify).

**Data Center cluster** (`index`, developer.atlassian.com): multiple active nodes sharing one relational DB; the Lucene index is replicated near-real-time with a copy local to each node; attachments in a shared location; cluster-wide locks; load balancer with sticky sessions. Requires a dedicated ops team; offers zero-downtime upgrades (7.3+), configurable REST rate limiting (8.6+), OpenSearch support.

**Search indexing** (`index`, confluence.atlassian.com): Jira builds a text index of issue-field content, updated on issue add/modify.
- **Lucene (filesystem):** per node under `<jira-home>/caches/indexes/` with sub-indexes `issues`, `comments`, `changes`, `entities/{portalpage,searchrequest}`.
- **OpenSearch:** one Issue index, alias `issues`, production index named `issue-atlas-<timestamp>`, rebuilt via a staging index for low-impact reindexing.
- Cross-node sync aims at **eventual consistency** — there is a delay before index changes are visible on other nodes. Reindexing can be background or full (full deletes and rebuilds everything).

**Key subsystems:**

- **Projects** — container for issues with a key (e.g. `PROJ`), lead, components, versions. Project types: Software (Scrum/Kanban boards), Service Management (JSM, SLA-backed queues), Business/team-managed.
- **Issue types** — Story/Task/Bug/Sub-task/Epic (+ custom); hierarchy managed via a unified `parent` field (Epic = level 1 parent, subtasks level −1; Premium/Advanced Roadmaps adds configurable higher levels) (`index`, third-party research corroborated across sources).
- **Workflows** — per project+issue-type mapping (workflow scheme). A workflow is statuses linked by transitions; transitions carry **conditions** (who/when a transition is available), **validators** (field requirements checked on submit), and **post-functions** (side effects: fire events, set fields, trigger notifications). REST must GET available transitions then POST the chosen one.
- **Screens** — define which fields appear on create/edit/view. Critical coupling: REST field updates are validated against screens.
- **Fields / custom fields** — system fields + admin-defined custom fields (`customfield_10020` style IDs), with types (text, select, date, user, sprint…), contexts, and options; discovery via `/rest/api/3/field`.
- **Permission schemes** — ~30+ project permissions (Browse, Create, Edit, Transition, Assign, Resolve…) granted to groups, project roles, or the assignee/reporter; plus global permissions (Administer Jira). REST calls are authorized per permission of the calling user (`verified live`).
- **Notification schemes** — map issue events (created/updated/commented…) to recipients (reporter, assignee, watchers, roles). In Cloud, most event mail is driven by these schemes plus automation rules.
- **Automation rules** — no-code trigger/condition/action engine (originally Code Barrel's Automation for Jira, acquired 2019). Usage limits: Jira Standard ~1,700 rule runs/month; Premium ~1,000 × users/month (`index`, community). Atlassian is moving pricing to per-step accounting.
- **Agile boards/sprints** — boards are saved JQL filters + column mapping; sprints managed through the **Agile REST API** (`/rest/agile/1.0/...`): boards, sprints, backlog, ranking. Note the asymmetry: REST has board-scoped endpoints that JQL itself cannot express (`index`).
- **Dashboards** — gadget-based; backed by saved filters/JQL.
- **Apps — Forge & Connect** (`index` + `verified live` auth docs):
  - **Forge** (strategic): Atlassian-hosted serverless functions; app declared in `manifest.yml`; auth via Forge scopes (`api.asApp()` / `api.asUser().requestJira(...)`); eventing via the `trigger` module (product events) and `webtrigger` (HTTP entry).
  - **Connect** (legacy, being deprecated): app runs on the vendor's infrastructure; registered via `atlassian-connect.json`; JWT auth; webhooks declared in the descriptor and scoped to app lifecycle.
  - Architectural boundary: **Cloud apps cannot run in-process** (no P2 plugins, no internal Java APIs, no direct DB access) — unlike Data Center (`index`, Atlassian Community).
- **Webhooks** (`index`): events include `issue_created/updated/deleted`, `comment_created/updated/deleted`, `issuelink_created/deleted`, `attachment_created/deleted`, `project_*`, `sprint_created/updated/closed/deleted`, `user_created/updated`. Registration via Admin UI or `POST /rest/webhooks/1.0/webhook` with `events[]`, optional per-registration **secret** (HMAC `X-Hub-Signature` verification), optional **JQL filter** for issue events (sprint/version events ignore JQL filters), and `excludeBody`. Connect-app webhooks are lifecycle-scoped; dynamically registered REST webhooks require periodic re-registration (community sources cite a 30-day expiry; treat exact lifetime as unverified).

### 2. Data model

**The issue object** (composite of `verified live` REST docs + `index` research):

| Field | Notes |
|---|---|
| `id`, `key` | Internal integer ID; human key `PROJ-123` (project key + sequence) |
| `summary`, `description` | v3: **ADF JSON** (Atlassian Document Format) for description/environment; v2: wiki markup. Single-line custom fields accept plain strings |
| `issuetype`, `project`, `status`, `priority`, `resolution` | Status belongs to the project's workflow; resolution set on resolve |
| `assignee`, `reporter`, `creator` | **accountId**-keyed users in Cloud (email hidden behind user-privacy) |
| `labels`, `components`, `fixVersions`, `versions` | Tagging, grouping, release association |
| `customfield_NNNNN` | Custom fields; type/schema discoverable via `/rest/api/3/field` and `createmeta` |
| `parent` / `subtasks` | Unified hierarchy: Epic → standard types → sub-tasks |
| `issuelinks` | Typed links (`blocks`, `is blocked by`, `relates to`, `duplicates`…), inward/outward |
| `comment[]`, `attachment[]` | Linear comment threads (ADF bodies); attachments via multipart upload (`X-Atlassian-Token: no-check` required) (`verified live`) |
| `worklog[]` | Time tracking entries |
| `watchers`, `votes` | Notification/interest |
| `changelog` | Field-level history (expandable, not returned by default) |

**Workflow model** — states (statuses) grouped in categories (To Do / In Progress / Done); transitions between statuses; each transition may define conditions (e.g., only assignee), validators (e.g., resolution required), post-functions (e.g., assign, notify, fire `issue_resolved` event). Workflow schemes map (project × issue type) → workflow. Scheme edits are admin/UI operations, not available to agents via the REST API in any practical sense (`index`, agent skill docs).

**SLA concepts (JSM)** (`index`): per-project SLA goals such as **Time to first response** and **Time to resolution**, each with start/stop/pause conditions (status-based), target durations per priority, and working calendars. Two goal types: *time limit* (fixed target, e.g. "resolve within 8h") and *negotiated date* (per-issue deadline field). JQL exposes them via `breached()` and `remaining("15m")` functions; automation has an **SLA threshold breached** trigger.

**JQL — what it can and cannot express:**

Expressible (`index`, widely documented):
```
project = PROJ AND status != Done AND assignee = currentUser() ORDER BY priority DESC
project = PROJ AND updated >= -7d AND type in (Story, Bug)
issue IN linkedIssues("PROJ-1") AND issueLinkType = "is blocked by"
"Time to resolution" <= remaining("15m") AND "Time to resolution" != breached()
```

Not expressible (`index`, Atlassian Community threads):
- **No subqueries/subfilters**: cannot nest one query inside another ("children of issues matching X"); recursive hierarchy traversal ("all descendants of this Epic/Feature") needs marketplace apps (ScriptRunner, JQL Search Extensions).
- **No joins on linked-issue fields**: can't ask "issues blocked by an issue whose priority = Critical" dynamically.
- **No aggregation**: JQL "simply finds issues, it does not report on what it finds" — no COUNT subtasks, no GROUP BY; workarounds copy aggregates into custom fields via automation.
- **No board scoping** (only via saved-filter workaround), no `ORDER BY` on computed fields; bulk operations capped at 1,000 results.

For an agent, each unexpressible hop becomes a client-side loop of paginated API calls — expensive under points-based rate limits.

### 3. API surface

**Versions** (`verified live`): v3 and v2 expose the same operations; v3 adds ADF support. Base: `https://<site>.atlassian.net/rest/api/3/` (Basic/API-token) or `https://api.atlassian.com/ex/jira/<cloudId>/rest/api/3/` (OAuth 2.0 3LO). Data Center/Server use v2.

**Key endpoints** (from the official v3 reference navigation, `verified live`):

| Area | Endpoints |
|---|---|
| Issues | `POST /issue` · `GET/PUT/DELETE /issue/{key}` · `GET /issue/{key}/editmeta` · `GET /issue/createmeta` |
| Search | `POST /search/jql` (JQL body; the old `GET/POST /search` is removed — `index` corroboration) · pagination `startAt/maxResults/total/isLast`, newer endpoints use `nextPageToken` |
| Transitions | `GET /issue/{key}/transitions` → `POST /issue/{key}/transitions` |
| Comments | `GET/POST /issue/{key}/comment` · `PUT/DELETE /issue/{key}/comment/{id}` |
| Attachments | `POST /issue/{key}/attachments` (multipart, `X-Atlassian-Token: no-check`) |
| Links | `POST /issueLink` · link types `/issueLinkType` |
| Workflows | `/workflow`, `/workflowscheme`, `/workflow/search`, transition rules |
| Webhooks | `/webhook` (register/list/delete) |
| Users/groups | `/user`, `/user/search`, `/group`, `/myself` |
| Schemes | permission/notification/screen/issue-type schemes |
| Agile | `/rest/agile/1.0/board`, `/board/{id}/sprint`, `/sprint/{id}/issue`, backlog, rank |
| Misc | `/dashboard`, `/filter`, Jira expressions (`/expression` — sandboxed expression language over issues, no changelog access) |

**Authentication** (`verified live`): API token + email via HTTP Basic (scripts/bots); OAuth 2.0 authorization-code (3LO) with granular scopes (`read:jira-work`, `write:jira-work`, granular `read:issue:jira` etc.); Forge (`asApp`/`asUser` with manifest scopes); Connect (JWT). REST honors the calling user's permissions.

**Rate limits** (`verified live`, official rate-limiting doc, last updated 2026-10-15): three independent systems, `429` + `Retry-After` on breach:
1. **Points-based hourly quota** — each call costs 1 base point + per-object costs (core domain reads 1 pt/object; identity/permission reads 2 pts; writes 1 pt each). Tiers: Global Pool **65,000 pts/hr shared across all tenants** (default); Per-Tenant Pool (by review): Free 65k, Standard 100k+10×users, Premium 130k+20×users, Enterprise 150k+30×users, capped 500k. Resets top of UTC hour. Enforcement of the points model began 2026-03-02.
2. **Burst limits** — per-second, per tenant, per API path.
3. **Per-issue write limits** — caps modification frequency of a single issue.
(Data Center has its own configurable REST rate limiting since 8.6.)

**Well-known developer pain points** (`index`):
- **Create-metadata lies**: `createmeta` declares the Sprint field as `array` but create rejects arrays ("Number value expected as the Sprint id"); `description` declared `string` in v3 but rejects non-ADF payloads.
- **Screen-coupled REST**: fields absent from the edit screen silently fail to update via REST — by design, but a trap for integrations.
- **Rate-limit signals unreliable**: `X-RateLimit-*` headers not consistently present; lower-layer 503s need the same backoff treatment as 429s.
- **API churn**: `GET/POST /search` removed in favor of `POST /search/jql`; expansion/pagination limits "may change without notice" (official docs).
- **Discovery friction**: custom fields are opaque `customfield_10020` IDs requiring `/field` lookups; the Agile API is a separate surface with its own conventions.
- **Search-endpoint asymmetry**: board/sprint scoping exists in REST but not in JQL.
- **Workflow/scheme administration is UI-only in practice** (not exposed to the MCP/Rovo surface), so agents cannot self-configure projects.

### 4. Why Jira is painful for AI agents (analysis grounded in the above)

1. **Human-first UI assumptions.** Project setup (permission/notification/workflow/screen schemes) is admin-click work in the web UI; agents cannot create a correctly-configured project end-to-end. Board configuration, SLA calendars, and automation rules are likewise UI artifacts.
2. **Verbose, schema-on-read data model.** An issue GET returns hundreds of fields; the meaningful schema (which custom fields exist, what values selects take) must be assembled from `/field`, `createmeta`, and `editmeta`. Custom-field identity is an opaque integer. ADF JSON is verbose for an LLM compared with markdown (Linear/GitHub use markdown).
3. **JQL verbosity and gaps.** No subqueries, no recursion, no aggregation — the queries an agent most needs ("all open descendants of this epic, with their blockers' priorities") require N+1 client-side loops, each burning points against tight hourly quotas.
4. **Permission-scheme complexity.** Authorization is a 30+ permission matrix × schemes × project roles; failures surface as 403s with little explanation, and REST inherits screen restrictions — a recipe for agents thrashing against invisible walls.
5. **Unstructured blobs.** Comments and descriptions are free text/ADF; acceptance criteria, plans, and test evidence are conventions, not schema. Attachments are binary blobs; "plans/artifacts as machine data" has no first-class representation (issue properties/entity properties exist but are app-scoped key-value stores, not queryable semantics).
6. **No first-class agent identity.** API tokens are human users; OAuth apps act as the authorizing user; Forge apps get an app user. There is no "agent" principal with claim/lease semantics, heartbeats, or run-scoped credentials — every agent looks like a person or an app, and audit trails conflate them.
7. **Rate limits punish agent loops.** Points quotas are designed for human-scale traffic; an agent doing breadth-first traversal (search → expand links → fetch changelogs) exhausts quotas fast, and header signals are unreliable.
8. **Workflow rigidity vs agent iteration.** Transitions are admin-defined gates with validators/conditions; an agent's natural loop (draft → verify → revise → verify) maps poorly onto linear status machines, and there's no eval/verification primitive — "Done" is a status, not a checkable predicate.

(Atlassian's answer so far: the official **Rovo MCP server** — `https://mcp.atlassian.com/v1/mcp`, beta May 2025, GA 2026-02-04, OAuth 2.1/API-token auth, Jira/Confluence/Compass/JSM tools, every call running as the authorizing user with their permissions (`index`, third-party reviews). It's a better on-ramp, but it wraps the same data model and permission semantics — it does not fix them.)

---

## PART 2 — "Jira for agents" design gaps and precedents

### 5. Open-source and API-first issue trackers — comparison

| Tracker | License | API style & auth | Core data model | Agent-first qualities | Notable gaps for agents |
|---|---|---|---|---|---|
| **Plane** | AGPL-3.0 (open source) | REST v1 `/api/v1/`; API key/PAT, OAuth less tested; 60 req/min/key; custom cursor `value:offset:is_prev` (`index`) | Workspace → Projects → Cycles (sprints) / Modules (milestones) / Views (saved filters) → **Issues** (work items); labels, states, priorities, estimates, sub-issue links (`index`) | Clean JSON REST, official Python SDK, non-expiring PATs, granular OAuth scopes (AXS benchmark rated auth 13/15) | Webhook reliability bugs (duplicate POSTs; API-created issues don't fire events — polling safer); issue **filtering silently broken** on public REST (`GET /work-items/` ignores filter params per Apr-2026 AXS test); inconsistent field names across write endpoints (`labels` vs `label_ids`, `description_html` vs `description`); Epics paywalled → Modules fallback |
| **OpenProject** | GPL-3.0 (open source) | REST **API v3 HAL/JSON** (hypermedia); OAuth2 + API keys; form-driven create/update with server validation; optimistic locking via `lockVersion` (`index`) | Projects → **Work packages** (types: task/bug/story/epic…); statuses/workflows, custom fields, hierarchies, time/cost tracking (`index`) | Mature query/filter API; explicit permissions/roles model good for agent identities; validation errors returned before commit | HAL verbosity is token-hostile (links everywhere); form-negotiation dance is many round trips; enterprise-shaped ceremony; weak real-time story |
| **Taiga** | MPL-2.0 (open source) | REST v1 `/api/v1`; token auth; simple CRUD for epics/user stories/tasks/issues, sprints, comments, attachments, wiki (`index`) | Projects with **Epics / User stories / Tasks / Issues** as separate first-class types; sprints; wiki (`index`) | Simplest mental model of the OSS set; **community MCP server exists** (mcp-taiga: 6 tools, 28 ops — precedent for MCP-first wrapping) | Fragmented work-item types (4 kinds to juggle); smaller ecosystem; no structured acceptance criteria; claim semantics absent |
| **YouTrack** | Proprietary (free tier; self-hostable) | REST `{base}/api/<entity>?fields=<f>&<filters>`; **permanent tokens** (Bearer) or OAuth2; `?fields=` projection on every call (`index`) | Projects → **Issues** with custom fields, state-machine workflows (+ JS rules), tags, links, work items (time), **commands API** (`Assignee me`, `work 2h …` natural-language) (`index`) | Field projection keeps payloads small; permanent tokens are ideal for headless agents; command API is LLM-natural | Proprietary — cannot fork; query language still stringly-typed; no MCP standard surface; agent identity = user token |
| **Linear** | Proprietary | **GraphQL-only** `https://api.linear.app/graphql` — same API the product uses internally; personal API key or OAuth2; complexity-based rate limits; cursor pagination (`index`) | Workspace → Teams; Initiative → **Project** → Issue → Sub-issue (10 levels); cycles, milestones; strongly-typed custom fields (`index`) | **Best API shape precedent**: sparse `issueUpdate(id, input)` partial updates; only `teamId`+`title` required to create; markdown descriptions; rich filters on `issues()` | Proprietary; GraphQL query construction is token-heavy for simple agents; no native acceptance-criteria schema; no claim/lease model |
| **GitHub Issues + Projects v2** | Proprietary | REST v3 issues + **GraphQL-only Projects v2**; fine-grained PATs; header-versioned API (`index`) | Repo → **Issues** (labels, assignees, milestones, types, sub-issues); Projects v2 boards with custom fields (status/priority/iteration live in the project, not the issue) (`index`) | Simplest CRUD in the industry; markdown everywhere; `/search/issues` qualifiers; **IssueMe** precedent (agent-first wrapper: bounded local JSON cache, structured tools, fail-fast on rate limits, no webhooks/telemetry) (`index`) | Workflow semantics split across REST (issues) and GraphQL (projects) — the "item-add-no-status" trap; no native priority/due-date on issues; no claim semantics; board fields require per-field mutations |

**Takeaway:** the closest *forkable* base is **Plane** (modern stack, clean REST, active community) or **Taiga** (simplest model, existing MCP precedent); the closest *API-shape* inspiration is **Linear** (sparse updates, same-API-the-product-uses) and **YouTrack** (`?fields=` projection). None offers agent identity, claim/lease work distribution, structured acceptance criteria, or eval hooks — those are the gaps to build.

### 6. What "for agents" should mean concretely

A tracker designed for AI coding agents should invert Jira's assumptions. Concretely:

1. **Work items as structured tasks.** Schema includes machine-readable **acceptance criteria** (a list of checkable predicates, e.g. `{type: "test_passes", ref: "tests/auth.spec.ts#login-lockout"}` or `{type: "file_exists", path: "…"}`), not a free-text description. Title/body stay human-readable, but the *contract* is structured JSON. Status is derived from criteria state where possible (all criteria met → eligible for `done`), not just a human's click.
2. **Agent identity and claim semantics.** Agents are first-class principals (`agent:{name}:{run_id}`), provisioned via short-lived tokens or mTLS, distinct from human users in audit logs. Work distribution follows **CI-runner semantics**: `POST /claims` — long-poll or atomic claim of the highest-priority ready task with a **lease** (TTL + heartbeat `PATCH /claims/{id}/heartbeat`); lease expiry auto-releases. No assignee-field races; claiming is atomic and server-mediated. Polling claim endpoint replaces "who's working on what" dashboards.
3. **MCP server as the primary interface.** The reference client is an MCP server exposing ~12 typed tools (`list_tasks`, `claim_task`, `update_task`, `add_artifact`, `transition`, `submit_eval`, `watch_events`…) with strict JSON schemas — no JQL-string building, no ADF wrangling. REST remains for humans/integrations, but the MCP surface is the designed-first contract (precedent: mcp-taiga community server; Atlassian's Rovo MCP as validation of the pattern).
4. **Event streaming.** State changes (created/claimed/transitioned/criteria-updated/artifact-added) published to an event bus and delivered over **SSE** (simple, HTTP-friendly, replayable with `Last-Event-ID`) and optionally websockets. This replaces webhook registration ceremonies and unreliable polling — agents subscribe to `stream?project=X&since=cursor`.
5. **Deterministic workflow state machines agents can drive via API.** Workflows are declared as code/data (e.g. a JSON/YAML statechart in the repo), versioned, and fully manageable via API — no UI-only scheme editing. Transitions are pure functions with declared preconditions (`all_criteria_met`, `artifact_present(kind="test_report")`); an agent can query "why can't I transition?" and get a machine-readable answer (`missing: [criterion c3]`).
6. **An LLM-friendly query language simpler than JQL.** A tiny predicate language over a fixed schema, e.g. `status:open assignee:none label:backend -label:wip updated:<7d`, plus structured JSON filter equivalents (`{"status":"open","labels":["backend"]}`). No subquery gap: relations are first-class (`children_of:X`, `blocked_by:X`, `descendants_of:X`) and the server resolves them — no client-side N+1.
7. **Plans/artifacts as first-class structured attachments.** Attachments carry a `kind` (`plan`, `test_report`, `diff`, `log`, `screenshot`, `eval_result`) and typed metadata (JSON schema per kind); content-addressed storage; plans are versioned objects agents can diff, not binary blobs. Acceptance criteria can reference artifacts (`artifact:kind=test_report,status=pass`).
8. **Multi-agent orchestration.** Parent tasks spawn **sub-agent tasks** with explicit handoff records (`handoff: {from, to, context_hash, artifacts}`); optimistic concurrency via per-task version/etag on every mutation; conflict policy declared per project (last-writer-wins vs merge-required); parallel sub-tasks can proceed without lock-step coordination.
9. **Eval hooks.** Transitions into terminal states can require an **eval**: run a declared check (command, test suite, or LLM-judge prompt with rubric) and record the result as an artifact; failure blocks the transition with structured reasons. This makes "Done" a verifiable predicate — the single biggest semantic gap vs Jira.
10. **Minimal human web UI as secondary.** Read-mostly: board/list views, task detail rendering the structured criteria and artifacts, audit trail. Humans approve/override; they don't administer scheme matrices. Admin is config files + API, not click-ops.

### 7. Suggested component breakdown (solo-dev buildable)

Target: one developer, ~3–6 months part-time. Boring technology: Python (FastAPI) or Go; Postgres; Redis optional; S3-compatible object storage (or local disk to start).

| # | Component | Responsibility | Build notes |
|---|---|---|---|
| 1 | **Issue store** | Postgres schema: `projects`, `tasks` (JSONB for criteria/artifacts metadata + normalized columns for indexed fields: status, assignee, labels, priority, updated), `task_links`, `comments`, `agents`, `claims`, `audit_log` (append-only) | Single migration set; JSONB + GIN indexes for criteria; keep the queryable surface fixed (don't repeat Jira's custom-field sprawl) |
| 2 | **REST/JSON API** | CRUD for projects/tasks/comments/artifacts, workflow admin (declare statecharts via YAML/JSON), the predicate query endpoint, user/agent management | FastAPI + Pydantic gives typed schemas free; OpenAPI docs double as the agent contract reference |
| 3 | **MCP server** | Primary agent interface: ~12 tools over the REST core (list/claim/update/transition/artifact/eval/stream) with strict JSON schemas | Thin wrapper over component 2's service layer, not over HTTP — stdio + streamable HTTP transports (precedent: mcp-taiga) |
| 4 | **Claim API + lease manager** | Atomic `claim_next(filters)` with lease TTL, heartbeats, auto-release on expiry, per-agent concurrency caps | Small state machine in Postgres (`claims` table, `SELECT … FOR UPDATE SKIP LOCKED`); heartbeat endpoint; a janitor job reaps expired leases |
| 5 | **Workflow engine** | Versioned statechart definitions; transition preconditions (`criteria_met`, `artifact_present`); machine-readable "why blocked" responses; eval-hook runner (shell command / test / LLM-judge) | Pure library, no UI; definitions stored as YAML in the repo and applied via API — the anti-Jira-scheme-editor |
| 6 | **Event bus + streaming** | Append state-change events to an outbox table; fan out to SSE endpoint with cursor replay; optional webhook delivery for external integrations | Postgres LISTEN/NOTIFY or a tiny in-process bus to start; SSE with `Last-Event-ID` — no Kafka needed at this scale |
| 7 | **Artifact storage** | Content-addressed blob store (SHA-256) with typed metadata (`kind`, schema-validated JSON sidecar), versioning, diff-friendly for plans | Local disk → S3-compatible later; artifacts referenced from criteria and evals |
| 8 | **Minimal web UI** | Read-mostly board + task detail (criteria checklist, artifacts, audit trail), human approve/override buttons | Server-rendered (HTMX) or a tiny React app; explicitly *not* an admin console |

**Build order for a solo dev:** 1 → 2 → 4 → 5 → 3 → 6 → 7 → 8. The MCP server (3) and streaming (6) are what make it "for agents"; the workflow engine with eval hooks (5) is the differentiator vs every tracker in §5.

---

## Could not verify / open questions

- **Jira Cloud's internal datastore and search engine** (Lucene/OpenSearch/Elasticsearch, relational vs NoSQL) — Atlassian does not publicly document this; only Data Center internals are documented.
- **Exact lifetime/expiry of dynamically registered REST webhooks** — community sources cite 30-day re-registration; Atlassian's current docs page was not read to confirm.
- **Jira Cloud per-second burst numeric thresholds** — official docs describe burst limiting per tenant per path but the observed excerpt did not include the numeric caps; third-party sources cite 100 req/s GET/POST, 50 req/s PUT/DELETE (unverified against official docs).
- **Atlassian's long-term Forge-vs-Connect timeline specifics** — Connect deprecation is corroborated by developer-community threads, but exact dates were not verified.
- **Plane/OpenProject/Taiga API details** rest on third-party research docs and community benchmarks (2026), not on live testing; Plane's filter-param bug and webhook reliability issues are reported by integrators, not by the vendors.
- **YouTrack/Linear/GitHub** are proprietary; internals beyond public API docs are out of scope.

## Sources

### Verified live (research browser, 2026-10-15)
- Atlassian rate-limiting docs — https://developer.atlassian.com/cloud/jira/platform/rate-limiting/ — three rate-limit systems; points cost model; 65k/hr Global Pool; per-tenant tier quotas; 429 + Retry-After.
- Atlassian REST API v3 reference intro — https://developer.atlassian.com/cloud/jira/platform/rest/v3/intro/ — v3 vs v2 (ADF), auth models (Forge/Connect/OAuth 3LO/Basic), permissions model, pagination (`startAt/maxResults/total/isLast`), `expand`, `X-Atlassian-Token: no-check` for multipart.

### Read via index (2026-10-15)
- Jira Data Center search indexing — https://confluence.atlassian.com/enterprise/jira-data-center-search-indexing-939499986.html — Lucene paths, OpenSearch `issue-atlas-<timestamp>`, eventual consistency.
- Clustering developer docs — https://developer.atlassian.com/server/jira/platform/developing-for-high-availability-and-clustering/ — shared DB, per-node index copies, shared attachments.
- Jira Cloud SOC3 doc — https://www.atlassian.com/dam/jcr:22245294-f17e-41cc-810d-e28e28c9d4c7/Jira-and-Confluence-Cloud-SOC3_1-Oct-2017_to_31-Mar-2018.pdf — S3 Media Platform, per-tenant segregation, DynamoDB mapping, Monarch/CPS/TCS provisioning.
- Cloud-readiness community article — https://community.atlassian.com/forums/Jira-articles/Assessing-Your-Jira-Data-Center-Apps-for-Cloud-Readiness/ba-p/3154724 — Cloud architectural differences; Data Center sunset 2029.
- jira-cloud.md integration research — https://github.com/scion-frontiers/farmtable/blob/HEAD/.design/integration-research/jira-cloud.md — issue key/ID, hierarchy, unified `parent`, ADF in v3.
- Jira API skill (auth, rate limits, pagination, `/search/jql`) — https://github.com/wbunker/skills-repo/blob/HEAD/jira-expert/SKILL.md ; https://github.com/kurry/jira-marketing-agents/blob/HEAD/skills/jira-cloud-rest/SKILL.md
- JQL limitation threads (no subqueries/recursion/aggregation/board search) — https://community.atlassian.com/forums/Jira-questions/Query-to-read-results-from-one-query-and-filter-further/qaq-p/1773923 ; https://community.atlassian.com/forums/Jira-questions/What-is-the-JQL-to-show-tickets-under-a-feature/qaq-p/2397694 ; https://community.atlassian.com/forums/Jira-questions/JQL-to-return-all-issues-in-a-list-of-epics/qaq-p/2169012
- Create-metadata vs create inconsistencies; screen-coupled REST — https://community.atlassian.com/forums/Jira-questions/Inconsistencies-between-the-Create-Issue-API-and-the/qaq-p/1570350 ; https://community.atlassian.com/forums/Jira-questions/Question-or-two-about-using-REST-API-to-update-JIRA-issues/qaq-p/245099
- Rate-limit header gaps / 503 handling — https://community.developer.atlassian.com/t/jira-cloud-api-rate-limits-and-missing-headers/94358
- Workflow/scheme admin via UI; MCP gaps — https://github.com/catkinkitkat/ama-opencode-skilltree/blob/HEAD/./.opencode/skills/engineering/devtooling/jira-expert/SKILL.md
- Easy Agile API limitations (Data Center) — https://help.easyagile.com/easy-agile-support/atlassian-server-api-limitations
- Webhook event catalog — https://docs.cloud.google.com/application-integration/docs/configure-jira-cloud-trigger ; setup guide — https://hookdeck.com/webhooks/platforms/guide-to-jira-webhooks-features-and-best-practices
- Agent-driven Jira automation pattern (Forge app identity, HMAC webhooks, cloudId tenant key) — https://github.com/aws-samples/sample-autonomous-cloud-coding-agents/blob/HEAD/docs/guides/JIRA_SETUP_GUIDE.md
- Atlassian Rovo MCP (GA 2026-02-04, `mcp.atlassian.com`) — https://www.scalekit.com/blog/jira-mcp-vs-api ; https://www.truefoundry.com/blog/atlassian-mcp-server
- Automation usage limits / SLA concepts — https://community.atlassian.com/forums/Jira-articles/Getting-started-with-Jira-automation/ba-p/2883104 ; https://www.empyra.com/blog/jira-automation-pricing
- Plane research — https://github.com/lunarnexus/cellos/blob/HEAD/docs/pm-tools-research.md ; AXS benchmark — https://github.com/sat3n/axs-framework/blob/HEAD/benchmarks/project-management/results/plane.md ; field-name quirks — https://github.com/thomastabs/apex/blob/HEAD/docs/plane-integration.md
- OpenProject evaluation — https://github.com/simonvanlaak/cyberneticagents/blob/HEAD/docs/technical/openproject_integration_evaluation_2026-02-15.md ; HAL API example — https://github.com/opf/openproject/blob/dev/docs/api/apiv3/example/README.md
- Taiga MCP server precedent — https://github.com/negoro26/mcp-taiga
- YouTrack docs/API patterns — https://github.com/nicolapucci/youtrack-documentation ; https://github.com/bloombooks/bloom-team-skills/blob/HEAD/youtrack-api/SKILL.md
- Linear API research — https://github.com/nabila0721/claude-skills/blob/HEAD/project-management/linear-expert/SKILL.md ; https://github.com/evanoman/taskbench/blob/HEAD/research-agentic-clis/CaseStudies/02%20-%20Linear%20MCP%20and%20API.md ; https://github.com/scion-frontiers/farmtable/blob/HEAD/.design/integration-research/linear.md
- GitHub Issues research — https://github.com/scion-frontiers/farmtable/blob/HEAD/.design/integration-research/github-issues.md ; IssueMe agent-first wrapper — https://github.com/senad-d/issueme

### Analysis / inference (not sourced claims)
- §4 (agent pain points) and §§6–7 (design recommendations, component breakdown, build order) are my synthesis from the documented facts above.
