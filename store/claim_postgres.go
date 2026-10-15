package store

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Postgres claim/lease implementation.
//
// Claim runs in one transaction: purge expired leases, pick one candidate
// with SELECT ... FOR UPDATE SKIP LOCKED, insert the lease, and read the
// work item back. Concurrent agents never block each other: losers skip
// locked rows instead of queueing on them.

func (s *PostgresStore) Claim(ctx context.Context, agentID string, f WorkItemFilter, ttl time.Duration) (WorkItem, Claim, error) {
	if err := ctx.Err(); err != nil {
		return WorkItem{}, Claim{}, err
	}
	ttl, err := validateTTL(ttl)
	if err != nil {
		return WorkItem{}, Claim{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return WorkItem{}, Claim{}, fmt.Errorf("store: begin: %w", err)
	}
	defer tx.Rollback(ctx)

	var exists bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM agents WHERE id = $1)`, agentID).Scan(&exists); err != nil {
		return WorkItem{}, Claim{}, fmt.Errorf("store: claim agent check: %w", err)
	}
	if !exists {
		return WorkItem{}, Claim{}, fmt.Errorf("%w: agent %s", ErrNotFound, agentID)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM claims WHERE expires_at <= now()`); err != nil {
		return WorkItem{}, Claim{}, fmt.Errorf("store: purge expired claims: %w", err)
	}

	args := []any{agentID, ttl.Seconds()}
	var conds []string
	add := func(cond string, v any) {
		args = append(args, v)
		conds = append(conds, cond+"$"+strconv.Itoa(len(args)))
	}
	status := f.Status
	if status == "" {
		status = DefaultClaimableStatus
	}
	add("w.status = ", status)
	if f.ProjectKey != "" {
		add("w.project_key = ", f.ProjectKey)
	}
	if f.AssigneeID != "" {
		add("w.assignee_id = ", f.AssigneeID)
	}
	if f.IssueType != "" {
		add("w.issue_type = ", f.IssueType)
	}
	if len(f.Labels) > 0 {
		add("w.labels @> ", f.Labels)
	}

	var c Claim
	err = tx.QueryRow(ctx, `
		WITH candidate AS (
			SELECT w.key FROM work_items w
			WHERE `+strings.Join(conds, " AND ")+`
			  AND NOT EXISTS (SELECT 1 FROM claims c WHERE c.work_item_key = w.key)
			ORDER BY w.key
			LIMIT 1
			FOR UPDATE SKIP LOCKED
		)
		INSERT INTO claims (work_item_key, agent_id, expires_at)
		SELECT key, $1, now() + make_interval(secs => $2) FROM candidate
		RETURNING work_item_key, agent_id, claimed_at, expires_at`,
		args...,
	).Scan(&c.WorkItemKey, &c.AgentID, &c.ClaimedAt, &c.ExpiresAt)
	if err != nil {
		if isNoRows(err) {
			_ = tx.Rollback(ctx)
			return WorkItem{}, Claim{}, ErrNoWorkAvailable
		}
		return WorkItem{}, Claim{}, fmt.Errorf("store: claim: %w", err)
	}
	var row workItemRow
	if err := tx.QueryRow(ctx,
		`SELECT `+workItemColumns+` FROM work_items WHERE key = $1`, c.WorkItemKey,
	).Scan(row.scan()...); err != nil {
		return WorkItem{}, Claim{}, fmt.Errorf("store: claim read back: %w", err)
	}
	w, err := row.workItem()
	if err != nil {
		return WorkItem{}, Claim{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return WorkItem{}, Claim{}, fmt.Errorf("store: claim commit: %w", err)
	}
	c.ClaimedAt = c.ClaimedAt.UTC()
	c.ExpiresAt = c.ExpiresAt.UTC()
	return w, c, nil
}

func (s *PostgresStore) RenewClaim(ctx context.Context, key, agentID string, ttl time.Duration) (Claim, error) {
	if err := ctx.Err(); err != nil {
		return Claim{}, err
	}
	ttl, err := validateTTL(ttl)
	if err != nil {
		return Claim{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Claim{}, fmt.Errorf("store: begin: %w", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `DELETE FROM claims WHERE expires_at <= now()`); err != nil {
		return Claim{}, fmt.Errorf("store: purge expired claims: %w", err)
	}
	var c Claim
	err = tx.QueryRow(ctx, `
		UPDATE claims SET expires_at = now() + make_interval(secs => $3)
		WHERE work_item_key = $1 AND agent_id = $2
		RETURNING work_item_key, agent_id, claimed_at, expires_at`,
		key, agentID, ttl.Seconds(),
	).Scan(&c.WorkItemKey, &c.AgentID, &c.ClaimedAt, &c.ExpiresAt)
	if err != nil {
		if !isNoRows(err) {
			return Claim{}, fmt.Errorf("store: renew claim: %w", err)
		}
		var holder string
		qerr := tx.QueryRow(ctx,
			`SELECT agent_id FROM claims WHERE work_item_key = $1`, key).Scan(&holder)
		_ = tx.Rollback(ctx)
		if qerr != nil {
			return Claim{}, fmt.Errorf("%w: claim on %s", ErrNotFound, key)
		}
		return Claim{}, fmt.Errorf("%w: %s held by %s", ErrClaimConflict, key, holder)
	}
	if err := tx.Commit(ctx); err != nil {
		return Claim{}, fmt.Errorf("store: renew commit: %w", err)
	}
	c.ClaimedAt = c.ClaimedAt.UTC()
	c.ExpiresAt = c.ExpiresAt.UTC()
	return c, nil
}

func (s *PostgresStore) ReleaseClaim(ctx context.Context, key, agentID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: begin: %w", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `DELETE FROM claims WHERE expires_at <= now()`); err != nil {
		return fmt.Errorf("store: purge expired claims: %w", err)
	}
	tag, err := tx.Exec(ctx,
		`DELETE FROM claims WHERE work_item_key = $1 AND agent_id = $2`, key, agentID)
	if err != nil {
		return fmt.Errorf("store: release claim: %w", err)
	}
	if tag.RowsAffected() == 0 {
		var holder string
		qerr := tx.QueryRow(ctx,
			`SELECT agent_id FROM claims WHERE work_item_key = $1`, key).Scan(&holder)
		_ = tx.Rollback(ctx)
		if qerr != nil {
			return fmt.Errorf("%w: claim on %s", ErrNotFound, key)
		}
		return fmt.Errorf("%w: %s held by %s", ErrClaimConflict, key, holder)
	}
	return tx.Commit(ctx)
}

func (s *PostgresStore) GetClaim(ctx context.Context, key string) (Claim, error) {
	if err := ctx.Err(); err != nil {
		return Claim{}, err
	}
	var c Claim
	err := s.pool.QueryRow(ctx, `
		SELECT work_item_key, agent_id, claimed_at, expires_at FROM claims
		WHERE work_item_key = $1 AND expires_at > now()`, key,
	).Scan(&c.WorkItemKey, &c.AgentID, &c.ClaimedAt, &c.ExpiresAt)
	if err != nil {
		if isNoRows(err) {
			return Claim{}, fmt.Errorf("%w: claim on %s", ErrNotFound, key)
		}
		return Claim{}, fmt.Errorf("store: get claim: %w", err)
	}
	c.ClaimedAt = c.ClaimedAt.UTC()
	c.ExpiresAt = c.ExpiresAt.UTC()
	return c, nil
}

func (s *PostgresStore) ListClaims(ctx context.Context, agentID string) ([]Claim, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `
		SELECT work_item_key, agent_id, claimed_at, expires_at FROM claims
		WHERE agent_id = $1 AND expires_at > now()
		ORDER BY expires_at`, agentID)
	if err != nil {
		return nil, fmt.Errorf("store: list claims: %w", err)
	}
	defer rows.Close()
	var out []Claim
	for rows.Next() {
		var c Claim
		if err := rows.Scan(&c.WorkItemKey, &c.AgentID, &c.ClaimedAt, &c.ExpiresAt); err != nil {
			return nil, fmt.Errorf("store: scan claim: %w", err)
		}
		c.ClaimedAt = c.ClaimedAt.UTC()
		c.ExpiresAt = c.ExpiresAt.UTC()
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list claims rows: %w", err)
	}
	return out, nil
}
