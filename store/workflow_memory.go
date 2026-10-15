package store

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

// defaultWorkflowDef is the built-in workflow, mirrored in 003_workflows.sql.
var defaultWorkflowDef = json.RawMessage(`{
	"states": [
		{"name": "todo", "initial": true},
		{"name": "doing"},
		{"name": "done", "terminal": true}
	],
	"transitions": [
		{"from": "todo", "to": "doing"},
		{"from": "doing", "to": "todo"},
		{"from": "doing", "to": "done", "require_criteria_met": true}
	]
}`)

// seedDefaultWorkflowLocked installs the built-in workflow. Callers hold s.mu.
func (s *MemoryStore) seedDefaultWorkflowLocked() {
	s.workflows["default"] = []StoredWorkflow{{
		ID:         "default",
		Version:    1,
		Name:       "Default",
		Definition: defaultWorkflowDef,
		CreatedAt:  time.Now().UTC(),
	}}
}

func (s *MemoryStore) CreateWorkflow(ctx context.Context, name string, definition json.RawMessage) (StoredWorkflow, error) {
	if err := ctx.Err(); err != nil {
		return StoredWorkflow{}, err
	}
	if err := validateDefinition(definition); err != nil {
		return StoredWorkflow{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	id := fmt.Sprintf("wf-%d", s.wfSeq.Add(1))
	w := StoredWorkflow{
		ID:         id,
		Version:    1,
		Name:       name,
		Definition: definition,
		CreatedAt:  time.Now().UTC(),
	}
	s.workflows[id] = []StoredWorkflow{w}
	return w, nil
}

func (s *MemoryStore) UpdateWorkflow(ctx context.Context, id string, definition json.RawMessage) (StoredWorkflow, error) {
	if err := ctx.Err(); err != nil {
		return StoredWorkflow{}, err
	}
	if err := validateDefinition(definition); err != nil {
		return StoredWorkflow{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	versions, ok := s.workflows[id]
	if !ok {
		return StoredWorkflow{}, fmt.Errorf("%w: workflow %s", ErrNotFound, id)
	}
	w := StoredWorkflow{
		ID:         id,
		Version:    versions[len(versions)-1].Version + 1,
		Name:       versions[0].Name,
		Definition: definition,
		CreatedAt:  time.Now().UTC(),
	}
	s.workflows[id] = append(versions, w)
	return w, nil
}

func (s *MemoryStore) GetWorkflow(ctx context.Context, id string, version int) (StoredWorkflow, error) {
	if err := ctx.Err(); err != nil {
		return StoredWorkflow{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	versions, ok := s.workflows[id]
	if !ok {
		return StoredWorkflow{}, fmt.Errorf("%w: workflow %s", ErrNotFound, id)
	}
	if version == 0 {
		return versions[len(versions)-1], nil
	}
	for _, w := range versions {
		if w.Version == version {
			return w, nil
		}
	}
	return StoredWorkflow{}, fmt.Errorf("%w: workflow %s version %d", ErrNotFound, id, version)
}

func (s *MemoryStore) ListWorkflows(ctx context.Context) ([]StoredWorkflow, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]StoredWorkflow, 0, len(s.workflows))
	for _, versions := range s.workflows {
		out = append(out, versions[len(versions)-1])
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (s *MemoryStore) SetProjectWorkflow(ctx context.Context, projectKey, workflowID string, version int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.projects[projectKey]; !ok {
		return fmt.Errorf("%w: project %s", ErrNotFound, projectKey)
	}
	versions, ok := s.workflows[workflowID]
	if !ok {
		return fmt.Errorf("%w: workflow %s", ErrNotFound, workflowID)
	}
	found := false
	for _, w := range versions {
		if w.Version == version {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("%w: workflow %s version %d", ErrNotFound, workflowID, version)
	}
	s.projectWorkflows[projectKey] = ProjectWorkflow{
		ProjectKey: projectKey,
		WorkflowID: workflowID,
		Version:    version,
	}
	return nil
}

func (s *MemoryStore) GetProjectWorkflow(ctx context.Context, projectKey string) (ProjectWorkflow, error) {
	if err := ctx.Err(); err != nil {
		return ProjectWorkflow{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	pw, ok := s.projectWorkflows[projectKey]
	if !ok {
		return ProjectWorkflow{}, fmt.Errorf("%w: workflow for project %s", ErrNotFound, projectKey)
	}
	return pw, nil
}

// pinDefaultWorkflowLocked pins a new project to the built-in workflow.
func (s *MemoryStore) pinDefaultWorkflowLocked(projectKey string) {
	s.projectWorkflows[projectKey] = ProjectWorkflow{
		ProjectKey: projectKey,
		WorkflowID: "default",
		Version:    1,
	}
}

func (s *MemoryStore) RecordEvalRun(ctx context.Context, run EvalRun) (EvalRun, error) {
	if err := ctx.Err(); err != nil {
		return EvalRun{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.items[run.WorkItemKey]; !ok {
		return EvalRun{}, fmt.Errorf("%w: work item %s", ErrNotFound, run.WorkItemKey)
	}
	run.ID = newEvalRunID(s.evalSeq.Add(1))
	run.RanAt = time.Now().UTC()
	s.evalRuns[run.ID] = run
	return run, nil
}

func (s *MemoryStore) ListEvalRuns(ctx context.Context, workItemKey string) ([]EvalRun, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []EvalRun
	for _, r := range s.evalRuns {
		if r.WorkItemKey == workItemKey {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RanAt.After(out[j].RanAt) })
	return out, nil
}
