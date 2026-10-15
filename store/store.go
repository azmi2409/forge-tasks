package store

import (
	"context"
	"errors"
	"time"
)

// Sentinel errors returned by Store implementations. Use errors.Is to match.
var (
	// ErrNotFound means no such project, work item, or agent.
	ErrNotFound = errors.New("store: not found")
	// ErrAlreadyExists means the key or ID is already taken.
	ErrAlreadyExists = errors.New("store: already exists")
	// ErrVersionConflict means the work item changed since it was read.
	// Callers should re-read and retry.
	ErrVersionConflict = errors.New("store: version conflict")
)

// Pagination bounds. Agents page aggressively; the store caps pages so one
// caller can't force a full-table scan into memory.
const (
	DefaultListLimit = 100
	MaxListLimit     = 1000
)

// WorkItemFilter selects work items for listing. Zero values mean "any".
type WorkItemFilter struct {
	ProjectKey string
	Status     string
	AssigneeID string
	IssueType  string
	Labels     []string // match items having ALL listed labels
	Limit      int      // <=0 means DefaultListLimit; capped at MaxListLimit
	Offset     int
}

// Store is the issue-store contract (component 1).
//
// Implementations must be safe for concurrent use and must deep-copy values
// across the boundary, so a caller can never mutate stored state through a
// previously returned value.
type Store interface {
	// CreateProject registers a project key prefix (e.g. FT).
	CreateProject(ctx context.Context, p Project) error
	// GetProject returns the project or ErrNotFound.
	GetProject(ctx context.Context, key string) (Project, error)

	// NextKey atomically allocates the next work-item key for a project
	// (e.g. "FT-124"). Safe under concurrency; keys are never reused, even
	// after deletes (gaps are acceptable, duplicates are not).
	NextKey(ctx context.Context, projectKey string) (string, error)

	// CreateWorkItem stores a new work item. An empty Key is allocated via
	// NextKey. Defaults are applied (status/issue_type/priority), Version
	// starts at 1. Returns ErrNotFound for an unknown project,
	// ErrAlreadyExists for a taken key.
	CreateWorkItem(ctx context.Context, w WorkItem) (WorkItem, error)
	// GetWorkItem returns the work item or ErrNotFound.
	GetWorkItem(ctx context.Context, key string) (WorkItem, error)
	// UpdateWorkItem applies w as the new state iff w.Version matches the
	// stored version (optimistic concurrency). On success the returned item
	// carries Version+1; on mismatch it returns ErrVersionConflict wrapping
	// the current version, so callers can re-read and retry.
	UpdateWorkItem(ctx context.Context, w WorkItem) (WorkItem, error)
	// DeleteWorkItem removes the work item. The key is never reallocated.
	DeleteWorkItem(ctx context.Context, key string) error
	// ListWorkItems returns matching items ordered by key, paged.
	ListWorkItems(ctx context.Context, f WorkItemFilter) ([]WorkItem, error)

	// UpsertAgent creates or replaces an agent principal.
	UpsertAgent(ctx context.Context, a AgentPrincipal) error
	// GetAgent returns the principal or ErrNotFound.
	GetAgent(ctx context.Context, id string) (AgentPrincipal, error)
	// Heartbeat records agent liveness. Unknown IDs return ErrNotFound.
	Heartbeat(ctx context.Context, id string, at time.Time) error
}
