# Deploying forge-tasks

## Local / single host (Docker Compose)

```bash
cp .env.example .env   # optional — defaults work out of the box
docker compose up -d
```

This starts Postgres 16 and the API service (REST + SSE event stream + web
UI on `:8080`). The API applies the embedded schema migrations on every boot
(`store/migrations/`: 001 issues, 002 claims, 003 workflows, 004 artifacts);
they are idempotent, so restarts and upgrades are safe.

Verify:

```bash
curl -s localhost:8080/healthz
# {"status":"ok"}

docker compose exec postgres psql -U forge -d forge_tasks -c '\dt'
# expect: agents, artifact_blobs, artifacts, claims, eval_runs,
#         key_counters, project_workflows, projects,
#         workflow_versions, workflows, work_items
```

What runs where:

| URL | What |
| --- | ---- |
| `http://localhost:8080/` | Web UI (human dashboard + item detail) |
| `http://localhost:8080/api/v1/...` | REST API (JSON) |
| `http://localhost:8080/api/v1/events` | SSE event stream |

The MCP server speaks stdio, so it isn't a long-lived service — run it
attached against the same database:

```bash
docker compose run --rm mcp
# or point any MCP client at it:
# { "command": "docker", "args": ["compose", "run", "--rm", "mcp"] }
```

Connection string (matches `DATABASE_URL` inside the compose network;
replace user/password/host when you set your own `.env`):

```
postgres://forge:forge@postgres:5432/forge_tasks?sslmode=disable
```

Tear down (keeps the data volume):

```bash
docker compose down
```

Tear down **including** data:

```bash
docker compose down -v
```

## Deploying on Coolify

Import the repo as a Docker Compose resource and deploy. If the deploy fails
with `Bind for :::8080 failed: port is already allocated`, another app on the
host already owns port 8080 — set a free host port in the resource's
environment variables, e.g. `API_PORT=18080`, and redeploy. Only the host
side of the mapping changes; the container still listens on 8080 internally,
so the healthcheck keeps working. Same trick applies to Postgres
(`POSTGRES_PORT`) if 5432 is taken.

`DATABASE_URL` is assembled from `POSTGRES_USER` / `POSTGRES_PASSWORD` /
`POSTGRES_DB` with plain (non-nested) variable defaults — nested
`${OUTER:-...${INNER}...}` syntax is intentionally avoided because Coolify's
parser closes the outer expression at the first `}` and corrupts the URL.
Keep passwords URL-safe (letters, numbers, `-`, `_`): characters like `@`,
`:`, `/`, `?`, `#` break the assembled URL, and `#` also truncates values in
`.env` files.

Without any `DATABASE_URL` the API silently runs on the in-memory store, so
always double-check the variable resolved if your data looks empty after a
deploy.

## Production notes

- Put Postgres behind your private network; don't expose 5432 publicly.
- Set a strong `POSTGRES_PASSWORD` via `.env` (never commit `.env`).
- The schema is designed for high concurrency: GIN indexes on JSONB/labels,
  partial index on assignee, and a `key_counters` table for atomic key
  allocation. Run one Postgres per region close to your API instances and
  scale the API horizontally — the store is stateless apart from the DB.
- Everything is currently unauthenticated (single-trust-domain MVP). Put the
  API behind your auth proxy / VPN before exposing it.
- The SSE stream holds one long-lived connection per subscriber; size your
  reverse proxy's idle timeouts accordingly (no response `WriteTimeout` is
  set server-side by design).
