# Deploying forge-tasks

## Local / single host (Docker Compose)

```bash
cp .env.example .env   # optional — defaults work out of the box
docker compose up -d
```

This starts Postgres 16 with the issue-store schema applied automatically on
first boot (`store/migrations/001_init.sql` is mounted into
`/docker-entrypoint-initdb.d`; the migration is idempotent).

Verify:

```bash
docker compose exec postgres psql -U forge -d forge_tasks -c '\dt'
# expect: agents, key_counters, projects, work_items
```

Connection string for the future API service:

```
postgres://forge:forge@postgres:5432/forge_tasks?sslmode=disable
```

(Replace user/password/host when you set your own `.env`.)

Tear down (keeps the data volume):

```bash
docker compose down
```

Tear down **including** data:

```bash
docker compose down -v
```

## Production notes

- Put Postgres behind your private network; don't expose 5432 publicly.
- Set a strong `POSTGRES_PASSWORD` via `.env` (never commit `.env`).
- The schema is designed for high concurrency: GIN indexes on JSONB/labels,
  partial index on assignee, and a `key_counters` table for atomic key
  allocation. Run one Postgres per region close to your API instances and
  scale the API horizontally — the store is stateless apart from the DB.
