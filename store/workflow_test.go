package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

var testWorkflowDef = json.RawMessage(`{
	"states": [
		{"name": "todo", "initial": true},
		{"name": "doing"},
		{"name": "done", "terminal": true}
	],
	"transitions": [
		{"from": "todo", "to": "doing"},
		{"from": "doing", "to": "done", "require_criteria_met": true}
	]
}`)

var badWorkflowDef = json.RawMessage(`{"states": [], "transitions": []}`)

func TestWorkflowCRUD(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()

	// Invalid definitions are rejected at the store boundary.
	if _, err := s.CreateWorkflow(ctx, "bad", badWorkflowDef); err == nil {
		t.Fatal("bad definition: got nil, want error")
	}

	w, err := s.CreateWorkflow(ctx, "Kanban", testWorkflowDef)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if w.Version != 1 || w.ID == "" {
		t.Fatalf("w=%+v", w)
	}
	if _, err := w.Parsed(); err != nil {
		t.Fatalf("parse back: %v", err)
	}

	// Update creates a new immutable version; v1 is untouched.
	w2, err := s.UpdateWorkflow(ctx, w.ID, testWorkflowDef)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if w2.Version != 2 {
		t.Fatalf("w2=%+v", w2)
	}
	v1, err := s.GetWorkflow(ctx, w.ID, 1)
	if err != nil || v1.Version != 1 {
		t.Fatalf("v1=%+v err=%v", v1, err)
	}
	latest, err := s.GetWorkflow(ctx, w.ID, 0)
	if err != nil || latest.Version != 2 {
		t.Fatalf("latest=%+v err=%v", latest, err)
	}
	if _, err := s.GetWorkflow(ctx, w.ID, 99); !errors.Is(err, ErrNotFound) {
		t.Fatalf("v99: got %v, want ErrNotFound", err)
	}
	if _, err := s.UpdateWorkflow(ctx, "nope", testWorkflowDef); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update missing: got %v, want ErrNotFound", err)
	}
	list, err := s.ListWorkflows(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	// default + Kanban.
	if len(list) != 2 {
		t.Fatalf("list=%+v", list)
	}
}

func TestProjectWorkflowPinning(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	if err := s.CreateProject(ctx, Project{Key: "FT", Name: "Forge"}); err != nil {
		t.Fatalf("project: %v", err)
	}
	// New projects start pinned to the default workflow.
	pw, err := s.GetProjectWorkflow(ctx, "FT")
	if err != nil {
		t.Fatalf("pin: %v", err)
	}
	if pw.WorkflowID != "default" || pw.Version != 1 {
		t.Fatalf("pw=%+v", pw)
	}
	def, err := s.GetWorkflow(ctx, "default", 1)
	if err != nil {
		t.Fatalf("default: %v", err)
	}
	if _, err := def.Parsed(); err != nil {
		t.Fatalf("default parse: %v", err)
	}

	w, err := s.CreateWorkflow(ctx, "Custom", testWorkflowDef)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.SetProjectWorkflow(ctx, "FT", w.ID, 1); err != nil {
		t.Fatalf("set: %v", err)
	}
	pw, err = s.GetProjectWorkflow(ctx, "FT")
	if err != nil || pw.WorkflowID != w.ID {
		t.Fatalf("pw=%+v err=%v", pw, err)
	}
	// Unknown workflow / version / project rejected.
	if err := s.SetProjectWorkflow(ctx, "FT", "nope", 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bad wf: got %v", err)
	}
	if err := s.SetProjectWorkflow(ctx, "FT", w.ID, 99); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bad version: got %v", err)
	}
	if err := s.SetProjectWorkflow(ctx, "NOPE", w.ID, 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bad project: got %v", err)
	}
}

func TestEvalRuns(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	if err := s.CreateProject(ctx, Project{Key: "FT", Name: "Forge"}); err != nil {
		t.Fatalf("project: %v", err)
	}
	w, err := s.CreateWorkItem(ctx, WorkItem{ProjectKey: "FT", Title: "task"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Unknown work item rejected.
	if _, err := s.RecordEvalRun(ctx, EvalRun{WorkItemKey: "FT-99"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bad item: got %v", err)
	}
	r1, err := s.RecordEvalRun(ctx, EvalRun{
		WorkItemKey: w.Key, Transition: "doing->done", HookName: "smoke",
		Command: "true", Passed: true, Output: "ok", DurationMs: 12,
	})
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	if r1.ID == "" || r1.RanAt.IsZero() {
		t.Fatalf("r1=%+v", r1)
	}
	time.Sleep(2 * time.Millisecond)
	r2, err := s.RecordEvalRun(ctx, EvalRun{
		WorkItemKey: w.Key, Transition: "doing->done", HookName: "smoke",
		Command: "true", Passed: false, ExitCode: 1, DryRun: true,
	})
	if err != nil {
		t.Fatalf("record2: %v", err)
	}
	if r1.ID == r2.ID {
		t.Fatal("duplicate eval run IDs")
	}
	runs, err := s.ListEvalRuns(ctx, w.Key)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(runs) != 2 || runs[0].ID != r2.ID {
		t.Fatalf("runs=%+v", runs)
	}
}
