package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/azmi2409/forge-tasks/bus"
)

const workflowColumns = `id, version, name, definition, created_at`

func scanWorkflow(row interface {
	Scan(...any) error
}) (StoredWorkflow, error) {
	var w StoredWorkflow
	var def []byte
	if err := row.Scan(&w.ID, &w.Version, &w.Name, &def, &w.CreatedAt); err != nil {
		return StoredWorkflow{}, err
	}
	w.Definition = json.RawMessage(def)
	w.CreatedAt = w.CreatedAt.UTC()
	return w, nil
}

func (s *PostgresStore) CreateWorkflow(ctx context.Context, name string, definition json.RawMessage) (StoredWorkflow, error) {
	if err := ctx.Err(); err != nil {
		return StoredWorkflow{}, err
	}
	if err := validateDefinition(definition); err != nil {
		return StoredWorkflow{}, err
	}
	var id string
	if err := s.pool.QueryRow(ctx,
		`SELECT 'wf-' || nextval('workflow_id_seq')`).Scan(&id); err != nil {
		return StoredWorkflow{}, fmt.Errorf("store: workflow id: %w", err)
	}
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO workflows (id, version, name, definition) VALUES ($1, 1, $2, $3)`,
		id, name, string(definition)); err != nil {
		return StoredWorkflow{}, fmt.Errorf("store: insert workflow: %w", err)
	}
	w, err := s.GetWorkflow(ctx, id, 1)
	if err != nil {
		return StoredWorkflow{}, err
	}
	s.publish(bus.TypeWorkflowCreated, w)
	return w, nil
}

func (s *PostgresStore) UpdateWorkflow(ctx context.Context, id string, definition json.RawMessage) (StoredWorkflow, error) {
	if err := ctx.Err(); err != nil {
		return StoredWorkflow{}, err
	}
	if err := validateDefinition(definition); err != nil {
		return StoredWorkflow{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return StoredWorkflow{}, fmt.Errorf("store: begin: %w", err)
	}
	defer tx.Rollback(ctx)
	var name string
	var maxVersion int
	err = tx.QueryRow(ctx,
		`SELECT name, version FROM workflows WHERE id = $1 ORDER BY version DESC LIMIT 1 FOR UPDATE`,
		id).Scan(&name, &maxVersion)
	if err != nil {
		if isNoRows(err) {
			return StoredWorkflow{}, fmt.Errorf("%w: workflow %s", ErrNotFound, id)
		}
		return StoredWorkflow{}, fmt.Errorf("store: lock workflow: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO workflows (id, version, name, definition) VALUES ($1, $2, $3, $4)`,
		id, maxVersion+1, name, string(definition)); err != nil {
		return StoredWorkflow{}, fmt.Errorf("store: insert workflow version: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return StoredWorkflow{}, fmt.Errorf("store: commit workflow version: %w", err)
	}
	w, err := s.GetWorkflow(ctx, id, maxVersion+1)
	if err != nil {
		return StoredWorkflow{}, err
	}
	s.publish(bus.TypeWorkflowUpdated, w)
	return w, nil
}

func (s *PostgresStore) GetWorkflow(ctx context.Context, id string, version int) (StoredWorkflow, error) {
	if err := ctx.Err(); err != nil {
		return StoredWorkflow{}, err
	}
	q := `SELECT ` + workflowColumns + ` FROM workflows WHERE id = $1 `
	args := []any{id}
	if version == 0 {
		q += `ORDER BY version DESC LIMIT 1`
	} else {
		q += `AND version = $2`
		args = append(args, version)
	}
	w, err := scanWorkflow(s.pool.QueryRow(ctx, q, args...))
	if err != nil {
		if isNoRows(err) {
			return StoredWorkflow{}, fmt.Errorf("%w: workflow %s", ErrNotFound, id)
		}
		return StoredWorkflow{}, fmt.Errorf("store: get workflow: %w", err)
	}
	return w, nil
}

func (s *PostgresStore) ListWorkflows(ctx context.Context) ([]StoredWorkflow, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT ON (id) `+workflowColumns+` FROM workflows ORDER BY id, version DESC`)
	if err != nil {
		return nil, fmt.Errorf("store: list workflows: %w", err)
	}
	defer rows.Close()
	var out []StoredWorkflow
	for rows.Next() {
		w, err := scanWorkflow(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan workflow: %w", err)
		}
		out = append(out, w)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list workflows rows: %w", err)
	}
	return out, nil
}

func (s *PostgresStore) SetProjectWorkflow(ctx context.Context, projectKey, workflowID string, version int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var exists bool
	if err := s.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM projects WHERE key = $1)`, projectKey).Scan(&exists); err != nil {
		return fmt.Errorf("store: project check: %w", err)
	}
	if !exists {
		return fmt.Errorf("%w: project %s", ErrNotFound, projectKey)
	}
	if err := s.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM workflows WHERE id = $1 AND version = $2)`,
		workflowID, version).Scan(&exists); err != nil {
		return fmt.Errorf("store: workflow check: %w", err)
	}
	if !exists {
		return fmt.Errorf("%w: workflow %s version %d", ErrNotFound, workflowID, version)
	}
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO project_workflows (project_key, workflow_id, workflow_version)
		VALUES ($1, $2, $3)
		ON CONFLICT (project_key) DO UPDATE SET
			workflow_id = EXCLUDED.workflow_id,
			workflow_version = EXCLUDED.workflow_version`,
		projectKey, workflowID, version); err != nil {
		return fmt.Errorf("store: set project workflow: %w", err)
	}
	s.publish(bus.TypeProjectWorkflowSet, ProjectWorkflow{
		ProjectKey: projectKey, WorkflowID: workflowID, Version: version,
	})
	return nil
}

func (s *PostgresStore) GetProjectWorkflow(ctx context.Context, projectKey string) (ProjectWorkflow, error) {
	if err := ctx.Err(); err != nil {
		return ProjectWorkflow{}, err
	}
	var pw ProjectWorkflow
	err := s.pool.QueryRow(ctx,
		`SELECT project_key, workflow_id, workflow_version FROM project_workflows WHERE project_key = $1`,
		projectKey).Scan(&pw.ProjectKey, &pw.WorkflowID, &pw.Version)
	if err != nil {
		if isNoRows(err) {
			return ProjectWorkflow{}, fmt.Errorf("%w: workflow for project %s", ErrNotFound, projectKey)
		}
		return ProjectWorkflow{}, fmt.Errorf("store: get project workflow: %w", err)
	}
	return pw, nil
}

func (s *PostgresStore) RecordEvalRun(ctx context.Context, run EvalRun) (EvalRun, error) {
	if err := ctx.Err(); err != nil {
		return EvalRun{}, err
	}
	var id string
	err := s.pool.QueryRow(ctx, `
		INSERT INTO eval_runs
			(id, work_item_key, transition, hook_name, command, passed,
			 exit_code, output, duration_ms, dry_run)
		VALUES ('eval-' || nextval('eval_run_seq'), $1, $2, $3, $4, $5, $6, $7, $8, $9)
		RETURNING id, ran_at`,
		run.WorkItemKey, run.Transition, run.HookName, run.Command, run.Passed,
		run.ExitCode, run.Output, run.DurationMs, run.DryRun,
	).Scan(&id, &run.RanAt)
	if err != nil {
		if isFKViolation(err) {
			return EvalRun{}, fmt.Errorf("%w: work item %s", ErrNotFound, run.WorkItemKey)
		}
		return EvalRun{}, fmt.Errorf("store: record eval run: %w", err)
	}
	run.ID = id
	run.RanAt = run.RanAt.UTC()
	s.publish(bus.TypeEvalRunRecorded, run)
	return run, nil
}

func (s *PostgresStore) ListEvalRuns(ctx context.Context, workItemKey string) ([]EvalRun, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, work_item_key, transition, hook_name, command, passed,
		       exit_code, output, duration_ms, dry_run, ran_at
		FROM eval_runs WHERE work_item_key = $1 ORDER BY ran_at DESC`, workItemKey)
	if err != nil {
		return nil, fmt.Errorf("store: list eval runs: %w", err)
	}
	defer rows.Close()
	var out []EvalRun
	for rows.Next() {
		var r EvalRun
		if err := rows.Scan(&r.ID, &r.WorkItemKey, &r.Transition, &r.HookName,
			&r.Command, &r.Passed, &r.ExitCode, &r.Output, &r.DurationMs,
			&r.DryRun, &r.RanAt); err != nil {
			return nil, fmt.Errorf("store: scan eval run: %w", err)
		}
		r.RanAt = r.RanAt.UTC()
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list eval runs rows: %w", err)
	}
	return out, nil
}
