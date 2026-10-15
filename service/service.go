// Package service is forge-tasks' business-logic layer (the "service layer"
// the REA prescribes): it orchestrates the store and the workflow engine.
// The REST API and the MCP server are both thin adapters over it — never
// over each other, and never over HTTP.
package service

import (
	"context"
	"fmt"

	"github.com/azmi2409/forge-tasks/bus"
	"github.com/azmi2409/forge-tasks/store"
	"github.com/azmi2409/forge-tasks/workflow"
)

// Service bundles the store with domain orchestration.
type Service struct {
	Store store.Store
	bus   *bus.Bus
}

// New returns a Service over the given store. The bus is optional; when
// non-nil, governed transitions publish transition.applied events.
func New(st store.Store, b *bus.Bus) *Service { return &Service{Store: st, bus: b} }

// SetBus attaches (or detaches, with nil) the event bus. Call before serving.
func (s *Service) SetBus(b *bus.Bus) { s.bus = b }

// TransitionOutcome is the result of a governed status change.
type TransitionOutcome struct {
	WorkItem   store.WorkItem
	Transition *workflow.Transition
	EvalRun    *store.EvalRun // non-nil when an eval hook ran
	DryRun     bool
}

// BlockedError is returned when a transition is refused. It carries the
// machine-readable reasons (never a plain string): callers render
// Result.BlockedCode / Missing / Available and the EvalRun.
type BlockedError struct {
	Result  workflow.Result
	EvalRun *store.EvalRun
}

func (e *BlockedError) Error() string {
	if e.Result.Message != "" {
		return "transition blocked: " + e.Result.Message
	}
	return "transition blocked: " + e.Result.BlockedCode
}

// LoadWorkflow resolves the pinned, parsed workflow for a project.
func (s *Service) LoadWorkflow(ctx context.Context, projectKey string) (workflow.Workflow, error) {
	pw, err := s.Store.GetProjectWorkflow(ctx, projectKey)
	if err != nil {
		return workflow.Workflow{}, err
	}
	sw, err := s.Store.GetWorkflow(ctx, pw.WorkflowID, pw.Version)
	if err != nil {
		return workflow.Workflow{}, err
	}
	return sw.Parsed()
}

// CriteriaView maps a work item's acceptance criteria onto the engine's view.
func CriteriaView(item store.WorkItem) []workflow.Criterion {
	out := make([]workflow.Criterion, 0, len(item.Acceptance))
	for _, c := range item.Acceptance {
		out = append(out, workflow.Criterion{ID: c.ID, Satisfied: c.Status == store.CriterionPassed})
	}
	return out
}

// AvailableTransitions lists the legal moves from the item's status.
func (s *Service) AvailableTransitions(ctx context.Context, key string) (string, []workflow.Transition, error) {
	item, err := s.Store.GetWorkItem(ctx, key)
	if err != nil {
		return "", nil, err
	}
	wf, err := s.LoadWorkflow(ctx, item.ProjectKey)
	if err != nil {
		return "", nil, err
	}
	return item.Status, wf.Available(item.Status), nil
}

// Transition applies a governed status change to item, which carries the
// caller's intended field state (sparse updates already applied) and the
// version they read. Steps: optimistic-concurrency check, precondition
// evaluation, eval-hook gate, then the update. With dryRun the eval hook
// still runs (so the answer is honest) but nothing is persisted.
//
// Error contract:
//   - store.ErrNotFound: unknown work item
//   - store.ErrVersionConflict: stale version (checked before evaluation)
//   - *BlockedError: refused, with machine-readable reasons
func (s *Service) Transition(ctx context.Context, item store.WorkItem, to string, dryRun bool) (TransitionOutcome, error) {
	stored, err := s.Store.GetWorkItem(ctx, item.Key)
	if err != nil {
		return TransitionOutcome{}, err
	}
	// Stale writes lose before any workflow evaluation: a client with an
	// old version has a stale view, so transition advice is meaningless.
	// (The store re-checks atomically at write time.)
	if item.Version != stored.Version {
		return TransitionOutcome{}, fmt.Errorf("%w: expected version %d, got %d",
			store.ErrVersionConflict, stored.Version, item.Version)
	}
	wf, err := s.LoadWorkflow(ctx, item.ProjectKey)
	if err != nil {
		return TransitionOutcome{}, err
	}
	// Evaluate against the caller's intended state: a single call may
	// satisfy criteria and transition together.
	result := wf.Evaluate(stored.Status, to, CriteriaView(item))
	if !result.Allowed {
		return TransitionOutcome{}, &BlockedError{Result: result}
	}
	evalRun, blocked := s.runEvalGate(ctx, item, stored.Status, result.Transition, dryRun)
	if blocked != nil {
		return TransitionOutcome{}, blocked
	}
	if dryRun {
		return TransitionOutcome{WorkItem: item, Transition: result.Transition, EvalRun: evalRun, DryRun: true}, nil
	}
	item.Status = to
	updated, err := s.Store.UpdateWorkItem(ctx, item)
	if err != nil {
		return TransitionOutcome{}, err
	}
	if s.bus != nil {
		s.bus.Publish(bus.TypeTransitionApplied, map[string]any{
			"key":     updated.Key,
			"from":    stored.Status,
			"to":      to,
			"version": updated.Version,
		})
	}
	return TransitionOutcome{WorkItem: updated, Transition: result.Transition, EvalRun: evalRun}, nil
}

// runEvalGate executes the transition's eval hook (if any) and records the
// run. A failing check becomes a *BlockedError, never a plain error.
func (s *Service) runEvalGate(ctx context.Context, item store.WorkItem, from string, tr *workflow.Transition, dryRun bool) (*store.EvalRun, *BlockedError) {
	if tr.Eval == nil {
		return nil, nil
	}
	res, err := workflow.RunEvalHook(ctx, *tr.Eval, workflow.EvalContext{
		WorkItemKey: item.Key,
		From:        from,
		To:          tr.To,
	})
	if err != nil {
		return nil, &BlockedError{Result: workflow.Result{
			BlockedCode: "eval_error",
			Message:     "eval hook could not run: " + err.Error(),
		}}
	}
	recorded, err := s.Store.RecordEvalRun(ctx, store.EvalRun{
		WorkItemKey: item.Key,
		Transition:  from + "->" + tr.To,
		HookName:    res.HookName,
		Command:     res.Command,
		Passed:      res.Passed && !res.TimedOut,
		ExitCode:    res.ExitCode,
		Output:      res.Output,
		DurationMs:  res.Duration.Milliseconds(),
		DryRun:      dryRun,
	})
	if err != nil {
		return nil, &BlockedError{Result: workflow.Result{
			BlockedCode: "eval_error",
			Message:     "could not record eval run: " + err.Error(),
		}}
	}
	// Best-effort: mirror the eval output as a versioned eval_result
	// artifact (component 7), so outputs are content-addressed and diffable.
	// A failure here never blocks the transition: the run is recorded.
	output := res.Output
	if output == "" {
		output = "(no output)"
	}
	_, _ = s.Store.CreateArtifact(ctx, store.ArtifactInput{
		Kind:        store.ArtifactEvalResult,
		Name:        "eval-" + res.HookName,
		WorkItemKey: item.Key,
		Content:     []byte(output),
		Metadata: map[string]any{
			"passed":      res.Passed && !res.TimedOut,
			"exit_code":   res.ExitCode,
			"hook_name":   res.HookName,
			"transition":  from + "->" + tr.To,
			"eval_run_id": recorded.ID,
			"dry_run":     dryRun,
		},
		CreatedBy: "workflow-engine",
	})
	switch {
	case res.TimedOut:
		return &recorded, &BlockedError{
			Result:  workflow.Result{BlockedCode: "eval_timeout", Message: "eval hook timed out"},
			EvalRun: &recorded,
		}
	case !res.Passed:
		return &recorded, &BlockedError{
			Result:  workflow.Result{BlockedCode: "eval_failed", Message: "eval hook failed"},
			EvalRun: &recorded,
		}
	}
	return &recorded, nil
}
