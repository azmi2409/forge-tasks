package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/azmi2409/forge-tasks/store"
)

const wfWithEval = `{
	"states": [
		{"name": "todo", "initial": true},
		{"name": "doing"},
		{"name": "done", "terminal": true}
	],
	"transitions": [
		{"from": "todo", "to": "doing"},
		{"from": "doing", "to": "done", "require_criteria_met": true,
		 "eval": {"name": "gate", "command": "CMD"}}
	]
}`

func TestTransitionFlow(t *testing.T) {
	s, _ := testServer(t)
	seedProject(t, s)
	doRequest(t, s, "POST", "/api/v1/work-items", map[string]any{
		"project_key": "FT",
		"title":       "task",
		"acceptance": []map[string]any{
			{"id": "c1", "description": "works", "predicate": "manual", "status": "pending"},
			{"id": "c2", "description": "tested", "predicate": "manual", "status": "pending"},
		},
	})

	// Discover available transitions.
	rec := doRequest(t, s, "GET", "/api/v1/work-items/FT-1/transitions", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list transitions: %d", rec.Code)
	}

	// todo -> doing: no preconditions.
	rec = doRequest(t, s, "POST", "/api/v1/work-items/FT-1/transitions", map[string]any{
		"to": "doing", "version": 1,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("transition: %d %s", rec.Code, rec.Body.String())
	}

	// doing -> done with unmet criteria: 422 + missing list.
	rec = doRequest(t, s, "POST", "/api/v1/work-items/FT-1/transitions", map[string]any{
		"to": "done", "version": 2,
	})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("blocked: %d, want 422", rec.Code)
	}
	var blocked struct {
		Error struct {
			Code    string   `json:"code"`
			Missing []string `json:"missing"`
		} `json:"error"`
	}
	decodeBody(t, rec, &blocked)
	if blocked.Error.Code != "criteria_not_met" || len(blocked.Error.Missing) != 2 {
		t.Fatalf("blocked=%+v", blocked)
	}

	// Unknown transition: 422 + available alternatives.
	rec = doRequest(t, s, "POST", "/api/v1/work-items/FT-1/transitions", map[string]any{
		"to": "bogus", "version": 2,
	})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("unknown: %d, want 422", rec.Code)
	}
	var unknown struct {
		Error struct {
			Code      string   `json:"code"`
			Available []string `json:"available"`
		} `json:"error"`
	}
	decodeBody(t, rec, &unknown)
	if unknown.Error.Code != "unknown_transition" || len(unknown.Error.Available) == 0 {
		t.Fatalf("unknown=%+v", unknown)
	}

	// Satisfy criteria via PATCH (acceptance update + no status change).
	rec = doRequest(t, s, "PATCH", "/api/v1/work-items/FT-1", map[string]any{
		"version": 2,
		"acceptance": []map[string]any{
			{"id": "c1", "description": "works", "predicate": "manual", "status": "passed"},
			{"id": "c2", "description": "tested", "predicate": "manual", "status": "passed"},
		},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("satisfy: %d %s", rec.Code, rec.Body.String())
	}

	// dry_run: allowed, but nothing applied.
	rec = doRequest(t, s, "POST", "/api/v1/work-items/FT-1/transitions", map[string]any{
		"to": "done", "version": 3, "dry_run": true,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("dry run: %d %s", rec.Code, rec.Body.String())
	}
	var dry struct {
		Allowed bool `json:"allowed"`
	}
	decodeBody(t, rec, &dry)
	if !dry.Allowed {
		t.Fatalf("dry=%+v", dry)
	}
	rec = doRequest(t, s, "GET", "/api/v1/work-items/FT-1", nil)
	var cur store.WorkItem
	decodeBody(t, rec, &cur)
	if cur.Status != "doing" {
		t.Fatalf("dry run applied! status=%s", cur.Status)
	}

	// Real transition now passes.
	rec = doRequest(t, s, "POST", "/api/v1/work-items/FT-1/transitions", map[string]any{
		"to": "done", "version": 3,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("done: %d %s", rec.Code, rec.Body.String())
	}
	decodeBody(t, rec, &struct{}{})
	rec = doRequest(t, s, "GET", "/api/v1/work-items/FT-1", nil)
	decodeBody(t, rec, &cur)
	if cur.Status != "done" || cur.Version != 4 {
		t.Fatalf("cur=%+v", cur)
	}

	// Terminal state: no outgoing transitions.
	rec = doRequest(t, s, "POST", "/api/v1/work-items/FT-1/transitions", map[string]any{
		"to": "doing", "version": 4,
	})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("terminal: %d, want 422", rec.Code)
	}
}

func TestPatchStatusGoesThroughWorkflow(t *testing.T) {
	s, _ := testServer(t)
	seedProject(t, s)
	doRequest(t, s, "POST", "/api/v1/work-items", map[string]any{
		"project_key": "FT", "title": "task",
		"acceptance": []map[string]any{
			{"id": "c1", "description": "x", "predicate": "manual", "status": "pending"},
		},
	})
	// todo -> doing via PATCH is fine (no preconditions).
	rec := doRequest(t, s, "PATCH", "/api/v1/work-items/FT-1", map[string]any{
		"version": 1, "status": "doing",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("patch doing: %d %s", rec.Code, rec.Body.String())
	}
	// PATCH straight to done bypasses nothing: criteria gate applies.
	rec = doRequest(t, s, "PATCH", "/api/v1/work-items/FT-1", map[string]any{
		"version": 2, "status": "done",
	})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("patch done: %d, want 422", rec.Code)
	}
	// One PATCH can satisfy criteria AND transition.
	rec = doRequest(t, s, "PATCH", "/api/v1/work-items/FT-1", map[string]any{
		"version": 2, "status": "done",
		"acceptance": []map[string]any{
			{"id": "c1", "description": "x", "predicate": "manual", "status": "passed"},
		},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("patch satisfy+done: %d %s", rec.Code, rec.Body.String())
	}
}

func TestEvalHookGate(t *testing.T) {
	s, _ := testServer(t)
	seedProject(t, s)

	mkWorkflow := func(cmd string) string {
		t.Helper()
		rec := doRequest(t, s, "POST", "/api/v1/workflows", map[string]any{
			"name":       "gated",
			"definition": json.RawMessage(strings.ReplaceAll(wfWithEval, `"CMD"`, strconv.Quote(cmd))),
		})
		if rec.Code != http.StatusCreated {
			t.Fatalf("create wf: %d %s", rec.Code, rec.Body.String())
		}
		var wf store.StoredWorkflow
		decodeBody(t, rec, &wf)
		rec = doRequest(t, s, "PUT", "/api/v1/projects/FT/workflow", map[string]any{
			"workflow_id": wf.ID, "version": 1,
		})
		if rec.Code != http.StatusNoContent {
			t.Fatalf("pin: %d", rec.Code)
		}
		return wf.ID
	}

	// Failing eval blocks the transition and records the run.
	mkWorkflow("false")
	doRequest(t, s, "POST", "/api/v1/work-items", map[string]any{"project_key": "FT", "title": "t"})
	doRequest(t, s, "POST", "/api/v1/work-items/FT-1/transitions", map[string]any{"to": "doing", "version": 1})
	rec := doRequest(t, s, "POST", "/api/v1/work-items/FT-1/transitions", map[string]any{"to": "done", "version": 2})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("eval fail: %d, want 422", rec.Code)
	}
	var blocked struct {
		Error struct {
			Code    string        `json:"code"`
			EvalRun store.EvalRun `json:"eval_run"`
		} `json:"error"`
	}
	decodeBody(t, rec, &blocked)
	if blocked.Error.Code != "eval_failed" {
		t.Fatalf("blocked=%+v", blocked)
	}
	if blocked.Error.EvalRun.HookName != "gate" || blocked.Error.EvalRun.Passed {
		t.Fatalf("eval_run=%+v", blocked.Error.EvalRun)
	}
	// History endpoint shows the failed run.
	rec = doRequest(t, s, "GET", "/api/v1/work-items/FT-1/evals", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("evals: %d", rec.Code)
	}
	var hist struct {
		EvalRuns []store.EvalRun `json:"eval_runs"`
	}
	decodeBody(t, rec, &hist)
	if len(hist.EvalRuns) != 1 || hist.EvalRuns[0].Passed {
		t.Fatalf("hist=%+v", hist)
	}

	// Passing eval lets it through.
	mkWorkflow("true")
	rec = doRequest(t, s, "POST", "/api/v1/work-items/FT-1/transitions", map[string]any{"to": "done", "version": 2})
	if rec.Code != http.StatusOK {
		t.Fatalf("eval pass: %d %s", rec.Code, rec.Body.String())
	}
}

func TestWorkflowAdmin(t *testing.T) {
	s, _ := testServer(t)
	seedProject(t, s)

	rec := doRequest(t, s, "POST", "/api/v1/workflows", map[string]any{
		"name":       "custom",
		"definition": json.RawMessage(testWorkflowDef),
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	var wf store.StoredWorkflow
	decodeBody(t, rec, &wf)

	// Bad definition rejected.
	rec = doRequest(t, s, "POST", "/api/v1/workflows", map[string]any{
		"name":       "bad",
		"definition": json.RawMessage(`{"states": []}`),
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad def: %d, want 400", rec.Code)
	}

	// Get latest + list.
	rec = doRequest(t, s, "GET", "/api/v1/workflows/"+wf.ID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get: %d", rec.Code)
	}
	rec = doRequest(t, s, "GET", "/api/v1/workflows", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d", rec.Code)
	}

	// Update -> version 2, then fetch v1 explicitly.
	rec = doRequest(t, s, "PUT", "/api/v1/workflows/"+wf.ID, map[string]any{
		"definition": json.RawMessage(testWorkflowDef),
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("update: %d", rec.Code)
	}
	var v2 store.StoredWorkflow
	decodeBody(t, rec, &v2)
	if v2.Version != 2 {
		t.Fatalf("v2=%+v", v2)
	}
	rec = doRequest(t, s, "GET", "/api/v1/workflows/"+wf.ID+"/versions/1", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get v1: %d", rec.Code)
	}

	// Pin the project, then read the pin back.
	rec = doRequest(t, s, "PUT", "/api/v1/projects/FT/workflow", map[string]any{
		"workflow_id": wf.ID, "version": 2,
	})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("pin: %d", rec.Code)
	}
	rec = doRequest(t, s, "GET", "/api/v1/projects/FT/workflow", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get pin: %d", rec.Code)
	}
	var pin struct {
		Pin store.ProjectWorkflow `json:"pin"`
	}
	decodeBody(t, rec, &pin)
	if pin.Pin.Version != 2 {
		t.Fatalf("pin=%+v", pin)
	}
}

const testWorkflowDef = `{
	"states": [
		{"name": "todo", "initial": true},
		{"name": "doing"},
		{"name": "done", "terminal": true}
	],
	"transitions": [
		{"from": "todo", "to": "doing"},
		{"from": "doing", "to": "done", "require_criteria_met": true}
	]
}`
