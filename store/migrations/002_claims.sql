-- forge-tasks component 4: claim/lease tables.
--
-- A claim is a lease of a work item by an agent. Expired leases are treated
-- as available; the claim transaction purges them lazily so no separate
-- reaper process is required for correctness.
--
-- High-load notes:
--   * The claim path uses SELECT ... FOR UPDATE SKIP LOCKED, so thousands of
--     agents racing for work serialize on nothing: losers skip locked rows
--     instead of blocking.
--   * idx_claims_expires_at keeps the lazy purge cheap.

CREATE TABLE IF NOT EXISTS claims (
    work_item_key TEXT PRIMARY KEY REFERENCES work_items(key) ON DELETE CASCADE,
    agent_id      TEXT NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    claimed_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at    TIMESTAMPTZ NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_claims_expires_at ON claims (expires_at);
CREATE INDEX IF NOT EXISTS idx_claims_agent_id ON claims (agent_id);
