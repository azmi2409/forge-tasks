package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PostgresStore is the production Store backed by Postgres + pgxpool.
//
// It implements the exact same contract as MemoryStore:
// per-project atomic key allocation via the key_counters table, optimistic
// concurrency via the version column, never-reused keys. The claim/lease API
// (component 3) will use SELECT ... FOR UPDATE SKIP LOCKED against these
// same tables.
type PostgresStore struct {
	pool *pgxpool.Pool
}

// NewPostgresStore connects to Postgres and verifies the schema is present.
// Run ApplyMigration first on a fresh database.
func NewPostgresStore(ctx context.Context, databaseURL string) (*PostgresStore, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("store: bad DATABASE_URL: %w", err)
	}
	// Pool sizing for agent fleets: many short-lived queries, so keep enough
	// connections without overwhelming Postgres.
	if cfg.MaxConns == 0 || cfg.MaxConns == 4 {
		cfg.MaxConns = 32
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("store: connect: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("store: ping: %w", err)
	}
	var ok bool
	err = pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = 'work_items')`,
	).Scan(&ok)
	if err != nil || !ok {
		pool.Close()
		return nil, fmt.Errorf("store: work_items table missing (run ApplyMigration)")
	}
	return &PostgresStore{pool: pool}, nil
}

// Close releases the pool.
func (s *PostgresStore) Close() { s.pool.Close() }

// Compile-time contract check.
var _ Store = (*PostgresStore)(nil)

func (s *PostgresStore) CreateProject(ctx context.Context, p Project) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := p.Validate(); err != nil {
		return err
	}
	// The seed_key_counter trigger also handles this; the explicit insert
	// keeps the adapter correct even if the trigger is ever dropped.
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: begin: %w", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx,
		`INSERT INTO projects (key, name) VALUES ($1, $2)`, p.Key, p.Name); err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("%w: project %s", ErrAlreadyExists, p.Key)
		}
		return fmt.Errorf("store: insert project: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO key_counters (project_key) VALUES ($1) ON CONFLICT (project_key) DO NOTHING`,
		p.Key); err != nil {
		return fmt.Errorf("store: seed counter: %w", err)
	}
	return tx.Commit(ctx)
}

func (s *PostgresStore) GetProject(ctx context.Context, key string) (Project, error) {
	if err := ctx.Err(); err != nil {
		return Project{}, err
	}
	var p Project
	err := s.pool.QueryRow(ctx,
		`SELECT key, name FROM projects WHERE key = $1`, key).Scan(&p.Key, &p.Name)
	if err != nil {
		if isNoRows(err) {
			return Project{}, fmt.Errorf("%w: project %s", ErrNotFound, key)
		}
		return Project{}, fmt.Errorf("store: get project: %w", err)
	}
	return p, nil
}

func (s *PostgresStore) NextKey(ctx context.Context, projectKey string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	// One round trip, row-locked: concurrent allocators serialize on the
	// counter row instead of on each other.
	var n int64
	err := s.pool.QueryRow(ctx,
		`UPDATE key_counters SET next_n = next_n + 1 WHERE project_key = $1 RETURNING next_n - 1`,
		projectKey).Scan(&n)
	if err != nil {
		if isNoRows(err) {
			if _, perr := s.GetProject(ctx, projectKey); perr != nil {
				return "", perr
			}
			return "", fmt.Errorf("store: key counter missing for project %s", projectKey)
		}
		return "", fmt.Errorf("store: next key: %w", err)
	}
	return fmt.Sprintf("%s-%d", projectKey, n), nil
}

func (s *PostgresStore) CreateWorkItem(ctx context.Context, w WorkItem) (WorkItem, error) {
	if err := ctx.Err(); err != nil {
		return WorkItem{}, err
	}
	if err := w.Validate(); err != nil {
		return WorkItem{}, err
	}
	key := w.Key
	if key == "" {
		var err error
		key, err = s.NextKey(ctx, w.ProjectKey)
		if err != nil {
			return WorkItem{}, err
		}
	} else if !strings.HasPrefix(key, w.ProjectKey+"-") {
		return WorkItem{}, &ValidationError{Field: "key", Message: "must start with project key prefix"}
	}
	applyDefaults(&w)
	acceptance, err := json.Marshal(nullIfNilSlice(w.Acceptance))
	if err != nil {
		return WorkItem{}, fmt.Errorf("store: marshal acceptance: %w", err)
	}
	custom, err := json.Marshal(nullIfNil(w.CustomFields))
	if err != nil {
		return WorkItem{}, fmt.Errorf("store: marshal custom fields: %w", err)
	}
	var out workItemRow
	err = s.pool.QueryRow(ctx, `
		INSERT INTO work_items
			(key, project_key, title, description, issue_type, status, priority,
			 assignee_id, acceptance, labels, custom_fields, version)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,1)
		RETURNING `+workItemColumns,
		key, w.ProjectKey, w.Title, w.Description, w.IssueType, w.Status, w.Priority,
		nullIfEmpty(w.AssigneeID), string(acceptance), emptyStrings(w.Labels), string(custom),
	).Scan(out.scan()...)
	if err != nil {
		if isUniqueViolation(err) {
			return WorkItem{}, fmt.Errorf("%w: work item %s", ErrAlreadyExists, key)
		}
		if isFKViolation(err) {
			return WorkItem{}, fmt.Errorf("%w: project %s", ErrNotFound, w.ProjectKey)
		}
		return WorkItem{}, fmt.Errorf("store: insert work item: %w", err)
	}
	return out.workItem()
}

func (s *PostgresStore) GetWorkItem(ctx context.Context, key string) (WorkItem, error) {
	if err := ctx.Err(); err != nil {
		return WorkItem{}, err
	}
	var out workItemRow
	err := s.pool.QueryRow(ctx,
		`SELECT `+workItemColumns+` FROM work_items WHERE key = $1`, key).Scan(out.scan()...)
	if err != nil {
		if isNoRows(err) {
			return WorkItem{}, fmt.Errorf("%w: work item %s", ErrNotFound, key)
		}
		return WorkItem{}, fmt.Errorf("store: get work item: %w", err)
	}
	return out.workItem()
}

func (s *PostgresStore) UpdateWorkItem(ctx context.Context, w WorkItem) (WorkItem, error) {
	if err := ctx.Err(); err != nil {
		return WorkItem{}, err
	}
	if strings.TrimSpace(w.Key) == "" {
		return WorkItem{}, &ValidationError{Field: "key", Message: "required"}
	}
	if err := w.Validate(); err != nil {
		return WorkItem{}, err
	}
	acceptance, err := json.Marshal(nullIfNilSlice(w.Acceptance))
	if err != nil {
		return WorkItem{}, fmt.Errorf("store: marshal acceptance: %w", err)
	}
	custom, err := json.Marshal(nullIfNil(w.CustomFields))
	if err != nil {
		return WorkItem{}, fmt.Errorf("store: marshal custom fields: %w", err)
	}
	var out workItemRow
	err = s.pool.QueryRow(ctx, `
		UPDATE work_items SET
			title = $2, description = $3, issue_type = $4, status = $5,
			priority = $6, assignee_id = $7, acceptance = $8, labels = $9,
			custom_fields = $10, version = version + 1
		WHERE key = $1 AND version = $11
		RETURNING `+workItemColumns,
		w.Key, w.Title, w.Description, w.IssueType, w.Status, w.Priority,
		nullIfEmpty(w.AssigneeID), string(acceptance), emptyStrings(w.Labels), string(custom),
		w.Version,
	).Scan(out.scan()...)
	if err != nil {
		if !isNoRows(err) {
			return WorkItem{}, fmt.Errorf("store: update work item: %w", err)
		}
		// Distinguish "never existed" from "changed under us".
		var exists bool
		if qerr := s.pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM work_items WHERE key = $1)`, w.Key).Scan(&exists); qerr != nil {
			return WorkItem{}, fmt.Errorf("store: update check: %w", qerr)
		}
		if !exists {
			return WorkItem{}, fmt.Errorf("%w: work item %s", ErrNotFound, w.Key)
		}
		return WorkItem{}, fmt.Errorf("%w: work item %s", ErrVersionConflict, w.Key)
	}
	return out.workItem()
}

func (s *PostgresStore) DeleteWorkItem(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	tag, err := s.pool.Exec(ctx, `DELETE FROM work_items WHERE key = $1`, key)
	if err != nil {
		return fmt.Errorf("store: delete work item: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: work item %s", ErrNotFound, key)
	}
	// Keys are never reallocated: key_counters keeps incrementing.
	return nil
}

func (s *PostgresStore) ListWorkItems(ctx context.Context, f WorkItemFilter) ([]WorkItem, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	limit := f.Limit
	if limit <= 0 {
		limit = DefaultListLimit
	}
	if limit > MaxListLimit {
		limit = MaxListLimit
	}
	offset := f.Offset
	if offset < 0 {
		offset = 0
	}
	var conds []string
	var args []any
	add := func(cond string, v any) {
		args = append(args, v)
		conds = append(conds, cond+"$"+strconv.Itoa(len(args)))
	}
	if f.ProjectKey != "" {
		add("project_key = ", f.ProjectKey)
	}
	if f.Status != "" {
		add("status = ", f.Status)
	}
	if f.AssigneeID != "" {
		add("assignee_id = ", f.AssigneeID)
	}
	if f.IssueType != "" {
		add("issue_type = ", f.IssueType)
	}
	if len(f.Labels) > 0 {
		add("labels @> ", f.Labels)
	}
	where := ""
	if len(conds) > 0 {
		where = "WHERE " + strings.Join(conds, " AND ")
	}
	args = append(args, limit, offset)
	q := `SELECT ` + workItemColumns + ` FROM work_items ` + where +
		` ORDER BY key LIMIT $` + strconv.Itoa(len(args)-1) + ` OFFSET $` + strconv.Itoa(len(args))
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list work items: %w", err)
	}
	defer rows.Close()
	var out []WorkItem
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var r workItemRow
		if err := rows.Scan(r.scan()...); err != nil {
			return nil, fmt.Errorf("store: scan work item: %w", err)
		}
		w, err := r.workItem()
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list rows: %w", err)
	}
	return out, nil
}

func (s *PostgresStore) UpsertAgent(ctx context.Context, a AgentPrincipal) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := a.Validate(); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO agents (id, name, kind, capabilities, last_heartbeat)
		VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (id) DO UPDATE SET
			name = EXCLUDED.name, kind = EXCLUDED.kind,
			capabilities = EXCLUDED.capabilities,
			last_heartbeat = EXCLUDED.last_heartbeat`,
		a.ID, a.Name, string(a.Kind), emptyStrings(a.Capabilities), a.LastHeartbeat.UTC())
	if err != nil {
		return fmt.Errorf("store: upsert agent: %w", err)
	}
	return nil
}

func (s *PostgresStore) GetAgent(ctx context.Context, id string) (AgentPrincipal, error) {
	if err := ctx.Err(); err != nil {
		return AgentPrincipal{}, err
	}
	var a AgentPrincipal
	var kind string
	err := s.pool.QueryRow(ctx,
		`SELECT id, name, kind, capabilities, last_heartbeat FROM agents WHERE id = $1`,
		id).Scan(&a.ID, &a.Name, &kind, &a.Capabilities, &a.LastHeartbeat)
	if err != nil {
		if isNoRows(err) {
			return AgentPrincipal{}, fmt.Errorf("%w: agent %s", ErrNotFound, id)
		}
		return AgentPrincipal{}, fmt.Errorf("store: get agent: %w", err)
	}
	a.Kind = AgentKind(kind)
	return a, nil
}

func (s *PostgresStore) Heartbeat(ctx context.Context, id string, at time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE agents SET last_heartbeat = $2 WHERE id = $1`, id, at.UTC())
	if err != nil {
		return fmt.Errorf("store: heartbeat: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: agent %s", ErrNotFound, id)
	}
	return nil
}

// workItemColumns lists every selected column in scan order.
const workItemColumns = `key, project_key, title, description, issue_type, status,
	priority, assignee_id, acceptance, labels, custom_fields, version, created_at, updated_at`

// workItemRow is the Postgres row shape; workItem() converts to the domain type.
type workItemRow struct {
	key, projectKey, title, description string
	issueType, status, priority         string
	assigneeID                          *string
	acceptance, customFields            string
	labels                              []string
	version                             int64
	createdAt, updatedAt                time.Time
}

func (r *workItemRow) scan() []any {
	return []any{
		&r.key, &r.projectKey, &r.title, &r.description,
		&r.issueType, &r.status, &r.priority, &r.assigneeID,
		&r.acceptance, &r.labels, &r.customFields,
		&r.version, &r.createdAt, &r.updatedAt,
	}
}

func (r *workItemRow) workItem() (WorkItem, error) {
	var acceptance []AcceptanceCriterion
	if err := json.Unmarshal([]byte(r.acceptance), &acceptance); err != nil {
		return WorkItem{}, fmt.Errorf("store: unmarshal acceptance: %w", err)
	}
	var custom map[string]any
	if err := json.Unmarshal([]byte(r.customFields), &custom); err != nil {
		return WorkItem{}, fmt.Errorf("store: unmarshal custom fields: %w", err)
	}
	w := WorkItem{
		Key:          r.key,
		ProjectKey:   r.projectKey,
		Title:        r.title,
		Description:  r.description,
		IssueType:    r.issueType,
		Status:       r.status,
		Priority:     r.priority,
		Acceptance:   acceptance,
		Labels:       r.labels,
		CustomFields: custom,
		Version:      r.version,
		CreatedAt:    r.createdAt.UTC(),
		UpdatedAt:    r.updatedAt.UTC(),
	}
	if r.assigneeID != nil {
		w.AssigneeID = *r.assigneeID
	}
	return w, nil
}

func applyDefaults(w *WorkItem) {
	if w.Status == "" {
		w.Status = DefaultStatus
	}
	if w.IssueType == "" {
		w.IssueType = DefaultIssueType
	}
	if w.Priority == "" {
		w.Priority = DefaultPriority
	}
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullIfNil(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}

// nullIfNilSlice normalizes a nil acceptance list to [] so the JSONB column
// stores [] instead of null.
func nullIfNilSlice(s []AcceptanceCriterion) []AcceptanceCriterion {
	if s == nil {
		return []AcceptanceCriterion{}
	}
	return s
}

// emptyStrings normalizes a nil string slice to []string{} because pgx
// encodes nil slices as NULL, which violates NOT NULL columns.
func emptyStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func isNoRows(err error) bool { return err == pgx.ErrNoRows }

func isUniqueViolation(err error) bool {
	return pgErrCode(err) == "23505"
}

func isFKViolation(err error) bool {
	return pgErrCode(err) == "23503"
}

func pgErrCode(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}
