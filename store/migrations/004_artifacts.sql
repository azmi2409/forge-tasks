-- forge-tasks component 7: artifact storage.
--
-- Content-addressed: blobs are deduplicated by SHA-256 in artifact_blobs.
-- Metadata records are versioned per (work_item_key, name): re-uploading a
-- plan creates a new version, so agents can diff plan v1 vs v2.

CREATE TABLE IF NOT EXISTS artifact_blobs (
    sha256   TEXT PRIMARY KEY,
    content  BYTEA NOT NULL,
    size     BIGINT NOT NULL
);

CREATE TABLE IF NOT EXISTS artifacts (
    id            TEXT PRIMARY KEY,
    sha256        TEXT NOT NULL REFERENCES artifact_blobs(sha256) ON DELETE RESTRICT,
    kind          TEXT NOT NULL,
    name          TEXT NOT NULL,
    work_item_key TEXT REFERENCES work_items(key) ON DELETE CASCADE,
    metadata      JSONB NOT NULL DEFAULT '{}',
    version       INT NOT NULL CHECK (version >= 1),
    size          BIGINT NOT NULL,
    created_by    TEXT NOT NULL DEFAULT '',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Expression index: versioning is per (work_item, name); global artifacts
-- (no work item) version per name.
CREATE UNIQUE INDEX IF NOT EXISTS uq_artifacts_version
    ON artifacts (COALESCE(work_item_key, ''), name, version);
CREATE INDEX IF NOT EXISTS idx_artifacts_work_item ON artifacts (work_item_key, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_artifacts_kind ON artifacts (kind);
CREATE SEQUENCE IF NOT EXISTS artifact_id_seq;
