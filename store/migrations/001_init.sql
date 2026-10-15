-- forge-tasks component 1: issue store schema (Postgres 14+).
--
-- High-load design notes:
--   * work_items.version enables optimistic concurrency. The claim/lease API
--     (component 3) layers SELECT ... FOR UPDATE SKIP LOCKED on top of it so
--     thousands of agents can race for work without serializing on one row.
--   * Work-item keys (FT-123) come from the key_counters table:
--         UPDATE key_counters SET next_n = next_n + 1
--          WHERE project_key = $1 RETURNING next_n - 1;
--     one round trip, row-locked, gap-tolerant (gaps OK, duplicates fatal).
--   * acceptance and custom_fields are JSONB with GIN indexes, because agents
--     query structured predicates and custom fields far more than humans
--     browse screens.
--   * Partial index on assignee_id: most agent queries are "my work".

CREATE TABLE IF NOT EXISTS projects (
    key  TEXT PRIMARY KEY CHECK (key ~ '^[A-Z0-9]+$'),
    name TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS key_counters (
    project_key TEXT PRIMARY KEY REFERENCES projects(key) ON DELETE CASCADE,
    next_n      BIGINT NOT NULL DEFAULT 1
);

CREATE TABLE IF NOT EXISTS agents (
    id             TEXT PRIMARY KEY,
    name           TEXT NOT NULL,
    kind           TEXT NOT NULL CHECK (kind IN ('agent', 'human')),
    capabilities   TEXT[] NOT NULL DEFAULT '{}',
    last_heartbeat TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_agents_heartbeat ON agents (last_heartbeat);

CREATE TABLE IF NOT EXISTS work_items (
    key           TEXT PRIMARY KEY, -- 'FT-123'
    project_key   TEXT NOT NULL REFERENCES projects(key) ON DELETE CASCADE,
    title         TEXT NOT NULL,
    description   TEXT NOT NULL DEFAULT '',
    issue_type    TEXT NOT NULL DEFAULT 'task',
    status        TEXT NOT NULL DEFAULT 'todo',
    priority      TEXT NOT NULL DEFAULT 'medium',
    assignee_id   TEXT REFERENCES agents(id) ON DELETE SET NULL,
    acceptance    JSONB NOT NULL DEFAULT '[]',
    labels        TEXT[] NOT NULL DEFAULT '{}',
    custom_fields JSONB NOT NULL DEFAULT '{}',
    version       BIGINT NOT NULL DEFAULT 1,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Hot paths for agent fleets: list-by-project+status, "my work", label
-- searches, and structured queries over JSONB.
CREATE INDEX IF NOT EXISTS idx_work_items_project_status
    ON work_items (project_key, status);
CREATE INDEX IF NOT EXISTS idx_work_items_assignee
    ON work_items (assignee_id) WHERE assignee_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_work_items_labels
    ON work_items USING GIN (labels);
CREATE INDEX IF NOT EXISTS idx_work_items_custom_fields
    ON work_items USING GIN (custom_fields);
CREATE INDEX IF NOT EXISTS idx_work_items_acceptance
    ON work_items USING GIN (acceptance);

-- Keep updated_at fresh without application code.
CREATE OR REPLACE FUNCTION set_updated_at() RETURNS trigger AS $$
BEGIN
    NEW.updated_at = now();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_work_items_updated_at ON work_items;
CREATE TRIGGER trg_work_items_updated_at
    BEFORE UPDATE ON work_items
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- Seed the counter whenever a project is created (the adapter also handles
-- this idempotently with INSERT ... ON CONFLICT DO NOTHING).
CREATE OR REPLACE FUNCTION seed_key_counter() RETURNS trigger AS $$
BEGIN
    INSERT INTO key_counters (project_key) VALUES (NEW.key)
    ON CONFLICT (project_key) DO NOTHING;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_projects_seed_counter ON projects;
CREATE TRIGGER trg_projects_seed_counter
    AFTER INSERT ON projects
    FOR EACH ROW EXECUTE FUNCTION seed_key_counter();
