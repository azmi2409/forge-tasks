-- forge-tasks component 5: workflow engine persistence.
--
-- Workflows are versioned and immutable: updating a workflow inserts a new
-- row, and projects pin (workflow_id, workflow_version), so in-flight work
-- items never have the rules change under them.
--
-- Eval runs are recorded per transition attempt (pass or fail). Component 7
-- (artifact storage) will adopt these rows as kind='eval_result' artifacts.

CREATE TABLE IF NOT EXISTS workflows (
    id         TEXT NOT NULL,
    version    INT NOT NULL CHECK (version >= 1),
    name       TEXT NOT NULL,
    definition JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (id, version)
);

CREATE TABLE IF NOT EXISTS project_workflows (
    project_key      TEXT PRIMARY KEY REFERENCES projects(key) ON DELETE CASCADE,
    workflow_id      TEXT NOT NULL,
    workflow_version INT NOT NULL,
    FOREIGN KEY (workflow_id, workflow_version) REFERENCES workflows(id, version)
);

CREATE TABLE IF NOT EXISTS eval_runs (
    id            TEXT PRIMARY KEY,
    work_item_key TEXT NOT NULL REFERENCES work_items(key) ON DELETE CASCADE,
    transition    TEXT NOT NULL,
    hook_name     TEXT NOT NULL,
    command       TEXT NOT NULL,
    passed        BOOLEAN NOT NULL,
    exit_code     INT NOT NULL,
    output        TEXT NOT NULL DEFAULT '',
    duration_ms   BIGINT NOT NULL DEFAULT 0,
    dry_run       BOOLEAN NOT NULL DEFAULT FALSE,
    ran_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_eval_runs_work_item ON eval_runs (work_item_key, ran_at DESC);

CREATE SEQUENCE IF NOT EXISTS workflow_id_seq;
CREATE SEQUENCE IF NOT EXISTS eval_run_seq;

-- Seed the default workflow (todo -> doing -> done, criteria-gated).
INSERT INTO workflows (id, version, name, definition) VALUES (
    'default', 1, 'Default',
    '{
        "states": [
            {"name": "todo", "initial": true},
            {"name": "doing"},
            {"name": "done", "terminal": true}
        ],
        "transitions": [
            {"from": "todo", "to": "doing"},
            {"from": "doing", "to": "todo"},
            {"from": "doing", "to": "done", "require_criteria_met": true}
        ]
    }'::jsonb
) ON CONFLICT (id, version) DO NOTHING;

-- Backfill: projects created before this migration get the default workflow.
INSERT INTO project_workflows (project_key, workflow_id, workflow_version)
SELECT key, 'default', 1 FROM projects
ON CONFLICT (project_key) DO NOTHING;
