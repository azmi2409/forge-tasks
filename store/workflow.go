package store

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/azmi2409/forge-tasks/workflow"
)

// StoredWorkflow is one immutable version of a workflow definition.
type StoredWorkflow struct {
	ID         string          `json:"id"`
	Version    int             `json:"version"`
	Name       string          `json:"name"`
	Definition json.RawMessage `json:"definition"`
	CreatedAt  time.Time       `json:"created_at"`
}

// Parsed compiles the stored definition via the workflow engine.
func (w StoredWorkflow) Parsed() (workflow.Workflow, error) {
	return workflow.Parse(w.Definition)
}

// ProjectWorkflow pins a project to one workflow version.
type ProjectWorkflow struct {
	ProjectKey string `json:"project_key"`
	WorkflowID string `json:"workflow_id"`
	Version    int    `json:"version"`
}

// EvalRun is a persisted eval-hook execution. Component 7 (artifact
// storage) will adopt these rows as kind='eval_result' artifacts.
type EvalRun struct {
	ID          string    `json:"id"`
	WorkItemKey string    `json:"work_item_key"`
	Transition  string    `json:"transition"` // "doing->done"
	HookName    string    `json:"hook_name"`
	Command     string    `json:"command"`
	Passed      bool      `json:"passed"`
	ExitCode    int       `json:"exit_code"`
	Output      string    `json:"output"`
	DurationMs  int64     `json:"duration_ms"`
	DryRun      bool      `json:"dry_run"`
	RanAt       time.Time `json:"ran_at"`
}

// validateDefinition rejects garbage before it reaches storage.
func validateDefinition(def json.RawMessage) error {
	if _, err := workflow.Parse(def); err != nil {
		return &ValidationError{Field: "definition", Message: err.Error()}
	}
	return nil
}

// newEvalRunID builds a store-side ID from a sequence value.
func newEvalRunID(seq int64) string {
	return fmt.Sprintf("eval-%d", seq)
}
