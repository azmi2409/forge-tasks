package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/azmi2409/forge-tasks/bus"
)

const artifactColumns = `id, sha256, kind, name, work_item_key, metadata, version, size, created_by, created_at`

func scanArtifact(row interface{ Scan(...any) error }) (Artifact, error) {
	var a Artifact
	var workItemKey *string
	var meta []byte
	if err := row.Scan(&a.ID, &a.SHA256, &a.Kind, &a.Name, &workItemKey, &meta,
		&a.Version, &a.Size, &a.CreatedBy, &a.CreatedAt); err != nil {
		return Artifact{}, err
	}
	if workItemKey != nil {
		a.WorkItemKey = *workItemKey
	}
	if len(meta) > 0 {
		if err := json.Unmarshal(meta, &a.Metadata); err != nil {
			return Artifact{}, fmt.Errorf("store: decode artifact metadata: %w", err)
		}
	}
	a.CreatedAt = a.CreatedAt.UTC()
	return a, nil
}

func (s *PostgresStore) CreateArtifact(ctx context.Context, in ArtifactInput) (Artifact, error) {
	if err := ctx.Err(); err != nil {
		return Artifact{}, err
	}
	if err := in.Validate(); err != nil {
		return Artifact{}, err
	}
	if in.WorkItemKey != "" {
		var exists bool
		if err := s.pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM work_items WHERE key = $1)`,
			in.WorkItemKey).Scan(&exists); err != nil {
			return Artifact{}, fmt.Errorf("store: work item check: %w", err)
		}
		if !exists {
			return Artifact{}, fmt.Errorf("%w: work item %s", ErrNotFound, in.WorkItemKey)
		}
	}
	sum := sha256Hex(in.Content)
	meta := in.Metadata
	if meta == nil {
		meta = map[string]any{}
	}
	metaRaw, err := json.Marshal(meta)
	if err != nil {
		return Artifact{}, fmt.Errorf("store: marshal artifact metadata: %w", err)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Artifact{}, fmt.Errorf("store: begin: %w", err)
	}
	defer tx.Rollback(ctx)

	// Content-addressed blob: dedupe by hash.
	if _, err := tx.Exec(ctx,
		`INSERT INTO artifact_blobs (sha256, content, size) VALUES ($1, $2, $3)
		 ON CONFLICT (sha256) DO NOTHING`,
		sum, in.Content, len(in.Content)); err != nil {
		return Artifact{}, fmt.Errorf("store: insert blob: %w", err)
	}
	var id string
	if err := tx.QueryRow(ctx, `SELECT 'art-' || nextval('artifact_id_seq')`).Scan(&id); err != nil {
		return Artifact{}, fmt.Errorf("store: artifact id: %w", err)
	}
	var version int
	err = tx.QueryRow(ctx, `
		SELECT COALESCE(MAX(version), 0) + 1 FROM artifacts
		WHERE COALESCE(work_item_key, '') = COALESCE($1, '') AND name = $2`,
		nullIfEmpty(in.WorkItemKey), in.Name).Scan(&version)
	if err != nil {
		return Artifact{}, fmt.Errorf("store: artifact version: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO artifacts
			(id, sha256, kind, name, work_item_key, metadata, version, size, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		id, sum, in.Kind, in.Name, nullIfEmpty(in.WorkItemKey), string(metaRaw),
		version, len(in.Content), in.CreatedBy); err != nil {
		return Artifact{}, fmt.Errorf("store: insert artifact: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Artifact{}, fmt.Errorf("store: commit artifact: %w", err)
	}
	a, err := s.GetArtifact(ctx, id)
	if err != nil {
		return Artifact{}, err
	}
	s.publish(bus.TypeArtifactCreated, a)
	return a, nil
}

func (s *PostgresStore) GetArtifact(ctx context.Context, id string) (Artifact, error) {
	if err := ctx.Err(); err != nil {
		return Artifact{}, err
	}
	a, err := scanArtifact(s.pool.QueryRow(ctx,
		`SELECT `+artifactColumns+` FROM artifacts WHERE id = $1`, id))
	if err != nil {
		if isNoRows(err) {
			return Artifact{}, fmt.Errorf("%w: artifact %s", ErrNotFound, id)
		}
		return Artifact{}, fmt.Errorf("store: get artifact: %w", err)
	}
	return a, nil
}

func (s *PostgresStore) GetArtifactContent(ctx context.Context, id string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var content []byte
	err := s.pool.QueryRow(ctx, `
		SELECT b.content FROM artifact_blobs b
		JOIN artifacts a ON a.sha256 = b.sha256
		WHERE a.id = $1`, id).Scan(&content)
	if err != nil {
		if isNoRows(err) {
			return nil, fmt.Errorf("%w: artifact %s", ErrNotFound, id)
		}
		return nil, fmt.Errorf("store: get artifact content: %w", err)
	}
	return content, nil
}

func (s *PostgresStore) ListArtifacts(ctx context.Context, f ArtifactFilter) ([]Artifact, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	q := `SELECT ` + artifactColumns + ` FROM artifacts WHERE 1=1`
	var args []any
	if f.WorkItemKey != "" {
		args = append(args, f.WorkItemKey)
		q += fmt.Sprintf(` AND work_item_key = $%d`, len(args))
	}
	if f.Kind != "" {
		args = append(args, f.Kind)
		q += fmt.Sprintf(` AND kind = $%d`, len(args))
	}
	q += ` ORDER BY created_at DESC`
	if f.Limit > 0 {
		args = append(args, f.Limit)
		q += fmt.Sprintf(` LIMIT $%d`, len(args))
	}
	if f.Offset > 0 {
		args = append(args, f.Offset)
		q += fmt.Sprintf(` OFFSET $%d`, len(args))
	}
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list artifacts: %w", err)
	}
	defer rows.Close()
	var out []Artifact
	for rows.Next() {
		a, err := scanArtifact(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan artifact: %w", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list artifacts rows: %w", err)
	}
	return out, nil
}
