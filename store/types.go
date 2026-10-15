// Package store implements forge-tasks component 1: the issue store.
//
// Design notes for high load (massive concurrent AI-agent workload):
//   - Every method takes a context.Context so callers can cancel and bound
//     latency; adapters are expected to be backed by a connection pool.
//   - Writes use optimistic concurrency: every WorkItem carries a Version that
//     must match on update, otherwise ErrVersionConflict. The claim/lease API
//     (component 3) builds atomic claims on this primitive with
//     SELECT ... FOR UPDATE SKIP LOCKED in Postgres.
//   - Work-item keys (FT-123) are allocated from per-project atomic counters,
//     so concurrent creates never collide and keys are never reused.
//   - Implementations must deep-copy values across the store boundary, so a
//     caller can never mutate stored state via a previously returned value.
package store

import (
	"strconv"
	"strings"
	"time"
)

// PredicateType identifies how an acceptance criterion is checked by a machine.
type PredicateType string

const (
	// PredicateTestPasses runs a test command; passes on exit 0.
	// Args: {"command": "go test ./..."}.
	PredicateTestPasses PredicateType = "test_passes"
	// PredicateCommandSucceeds runs an arbitrary command; passes on exit 0.
	// Args: {"command": "..."}.
	PredicateCommandSucceeds PredicateType = "command_succeeds"
	// PredicateFileExists passes when the path exists. Args: {"path": "..."}.
	PredicateFileExists PredicateType = "file_exists"
	// PredicateManual has no machine check; a principal asserts it.
	PredicateManual PredicateType = "manual"
)

// ValidPredicate reports whether p is a known predicate type.
func ValidPredicate(p PredicateType) bool {
	switch p {
	case PredicateTestPasses, PredicateCommandSucceeds, PredicateFileExists, PredicateManual:
		return true
	}
	return false
}

// CriterionStatus is the evaluation state of one acceptance criterion.
type CriterionStatus string

const (
	CriterionPending CriterionStatus = "pending"
	CriterionPassed  CriterionStatus = "passed"
	CriterionFailed  CriterionStatus = "failed"
)

// AcceptanceCriterion is one machine-checkable acceptance predicate on a work
// item. This is the core "for agents" primitive: done-ness is verifiable,
// not a status label an agent asserts on faith.
type AcceptanceCriterion struct {
	ID            string          `json:"id"`
	Description   string          `json:"description"`
	Predicate     PredicateType   `json:"predicate"`
	PredicateArgs map[string]any  `json:"predicate_args,omitempty"`
	Status        CriterionStatus `json:"status"`
	EvaluatedAt   *time.Time      `json:"evaluated_at,omitempty"`
	EvaluatedBy   string          `json:"evaluated_by,omitempty"` // principal ID
}

// Validate checks the criterion's shape.
func (c AcceptanceCriterion) Validate() error {
	if strings.TrimSpace(c.Description) == "" {
		return &ValidationError{Field: "description", Message: "required"}
	}
	if !ValidPredicate(c.Predicate) {
		return &ValidationError{Field: "predicate", Message: "unknown predicate type"}
	}
	return nil
}

// AgentKind distinguishes agent principals from humans.
type AgentKind string

const (
	AgentKindAgent AgentKind = "agent"
	AgentKindHuman AgentKind = "human"
)

// AgentPrincipal is a first-class identity that can own work. Agents are not
// second-class API tokens here; they are principals with heartbeats.
type AgentPrincipal struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	Kind          AgentKind `json:"kind"`
	Capabilities  []string  `json:"capabilities,omitempty"`
	LastHeartbeat time.Time `json:"last_heartbeat"`
}

// Validate checks the principal's shape.
func (a AgentPrincipal) Validate() error {
	if strings.TrimSpace(a.ID) == "" {
		return &ValidationError{Field: "id", Message: "required"}
	}
	if a.Kind != AgentKindAgent && a.Kind != AgentKindHuman {
		return &ValidationError{Field: "kind", Message: "must be agent or human"}
	}
	return nil
}

// Project groups work items under a key prefix (e.g. FT).
type Project struct {
	Key  string `json:"key"`
	Name string `json:"name"`
}

// Validate checks the project's shape.
func (p Project) Validate() error {
	if strings.TrimSpace(p.Key) == "" {
		return &ValidationError{Field: "key", Message: "required"}
	}
	for _, r := range p.Key {
		if !(r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			return &ValidationError{Field: "key", Message: "must be uppercase alphanumeric"}
		}
	}
	return nil
}

// Defaults applied on work-item create.
const (
	DefaultStatus    = "todo"
	DefaultIssueType = "task"
	DefaultPriority  = "medium"
)

// WorkItem is the structured task: the unit of work an agent claims and completes.
type WorkItem struct {
	Key          string                `json:"key"` // e.g. "FT-123"; allocated if empty
	ProjectKey   string                `json:"project_key"`
	Title        string                `json:"title"`
	Description  string                `json:"description,omitempty"`
	IssueType    string                `json:"issue_type"`
	Status       string                `json:"status"`
	Priority     string                `json:"priority"`
	AssigneeID   string                `json:"assignee_id,omitempty"`
	Acceptance   []AcceptanceCriterion `json:"acceptance,omitempty"`
	Labels       []string              `json:"labels,omitempty"`
	CustomFields map[string]any        `json:"custom_fields,omitempty"`
	Version      int64                 `json:"version"` // optimistic concurrency token
	CreatedAt    time.Time             `json:"created_at"`
	UpdatedAt    time.Time             `json:"updated_at"`
}

// Validate checks the work item's shape (not its key allocation).
func (w WorkItem) Validate() error {
	if strings.TrimSpace(w.Title) == "" {
		return &ValidationError{Field: "title", Message: "required"}
	}
	if strings.TrimSpace(w.ProjectKey) == "" {
		return &ValidationError{Field: "project_key", Message: "required"}
	}
	for i, c := range w.Acceptance {
		if err := c.Validate(); err != nil {
			return &ValidationError{
				Field:   "acceptance[" + strconv.Itoa(i) + "]." + err.(*ValidationError).Field,
				Message: err.(*ValidationError).Message,
			}
		}
	}
	return nil
}

// ValidationError is returned when a value fails Validate.
type ValidationError struct {
	Field   string
	Message string
}

func (e *ValidationError) Error() string { return e.Field + ": " + e.Message }
