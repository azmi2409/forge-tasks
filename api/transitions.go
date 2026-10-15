package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/azmi2409/forge-tasks/store"
	"github.com/azmi2409/forge-tasks/workflow"
)

// --- workflow admin ---

func (s *Server) handleCreateWorkflow(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name       string          `json:"name"`
		Definition json.RawMessage `json:"definition"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		writeError(w, http.StatusBadRequest, "validation_error", "name: required")
		return
	}
	if len(req.Definition) == 0 {
		writeError(w, http.StatusBadRequest, "validation_error", "definition: required")
		return
	}
	wf, err := s.store.CreateWorkflow(r.Context(), req.Name, req.Definition)
	if !mapError(w, err) {
		return
	}
	writeJSON(w, http.StatusCreated, wf)
}

func (s *Server) handleListWorkflows(w http.ResponseWriter, r *http.Request) {
	wfs, err := s.store.ListWorkflows(r.Context())
	if !mapError(w, err) {
		return
	}
	if wfs == nil {
		wfs = []store.StoredWorkflow{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"workflows": wfs})
}

func (s *Server) handleGetWorkflow(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	version := 0
	if v := r.PathValue("version"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			writeError(w, http.StatusBadRequest, "validation_error", "version: positive integer required")
			return
		}
		version = n
	}
	wf, err := s.store.GetWorkflow(r.Context(), id, version)
	if !mapError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, wf)
}

func (s *Server) handleUpdateWorkflow(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Definition json.RawMessage `json:"definition"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if len(req.Definition) == 0 {
		writeError(w, http.StatusBadRequest, "validation_error", "definition: required")
		return
	}
	wf, err := s.store.UpdateWorkflow(r.Context(), r.PathValue("id"), req.Definition)
	if !mapError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, wf)
}

func (s *Server) handleSetProjectWorkflow(w http.ResponseWriter, r *http.Request) {
	var req struct {
		WorkflowID string `json:"workflow_id"`
		Version    int    `json:"version"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.WorkflowID) == "" || req.Version < 1 {
		writeError(w, http.StatusBadRequest, "validation_error",
			"workflow_id and a positive version are required")
		return
	}
	if err := s.store.SetProjectWorkflow(r.Context(), r.PathValue("key"), req.WorkflowID, req.Version); !mapError(w, err) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleGetProjectWorkflow(w http.ResponseWriter, r *http.Request) {
	pw, err := s.store.GetProjectWorkflow(r.Context(), r.PathValue("key"))
	if !mapError(w, err) {
		return
	}
	wf, err := s.store.GetWorkflow(r.Context(), pw.WorkflowID, pw.Version)
	if !mapError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"pin": pw, "workflow": wf})
}

// --- transitions ---

// loadWorkflow resolves the pinned, parsed workflow for an item's project.
func (s *Server) loadWorkflow(r *http.Request, projectKey string) (workflow.Workflow, error) {
	pw, err := s.store.GetProjectWorkflow(r.Context(), projectKey)
	if err != nil {
		return workflow.Workflow{}, err
	}
	sw, err := s.store.GetWorkflow(r.Context(), pw.WorkflowID, pw.Version)
	if err != nil {
		return workflow.Workflow{}, err
	}
	return sw.Parsed()
}

func criteriaView(item store.WorkItem) []workflow.Criterion {
	out := make([]workflow.Criterion, 0, len(item.Acceptance))
	for _, c := range item.Acceptance {
		out = append(out, workflow.Criterion{ID: c.ID, Satisfied: c.Status == store.CriterionPassed})
	}
	return out
}

// writeBlocked answers "why can't I transition?" in machine-readable form.
func writeBlocked(w http.ResponseWriter, result workflow.Result, evalRun *store.EvalRun) {
	body := map[string]any{
		"code":    result.BlockedCode,
		"message": result.Message,
	}
	if len(result.Missing) > 0 {
		body["missing"] = result.Missing
	}
	if len(result.Available) > 0 {
		body["available"] = result.Available
	}
	if evalRun != nil {
		body["eval_run"] = evalRun
	}
	writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": body})
}

// runEvalGate executes the transition's eval hook (if any) and records the
// run. A failing check is a *blocked transition*, not a server error.
func (s *Server) runEvalGate(r *http.Request, item store.WorkItem, tr *workflow.Transition, dryRun bool) (*store.EvalRun, *workflow.Result) {
	if tr.Eval == nil {
		return nil, nil
	}
	res, err := workflow.RunEvalHook(r.Context(), *tr.Eval, workflow.EvalContext{
		WorkItemKey: item.Key,
		From:        item.Status,
		To:          tr.To,
	})
	if err != nil {
		// The check itself couldn't run: treat as blocked with reasons.
		return nil, &workflow.Result{
			BlockedCode: "eval_error",
			Message:     "eval hook could not run: " + err.Error(),
		}
	}
	recorded, err := s.store.RecordEvalRun(r.Context(), store.EvalRun{
		WorkItemKey: item.Key,
		Transition:  item.Status + "->" + tr.To,
		HookName:    res.HookName,
		Command:     res.Command,
		Passed:      res.Passed && !res.TimedOut,
		ExitCode:    res.ExitCode,
		Output:      res.Output,
		DurationMs:  res.Duration.Milliseconds(),
		DryRun:      dryRun,
	})
	if err != nil {
		return nil, &workflow.Result{
			BlockedCode: "eval_error",
			Message:     "could not record eval run: " + err.Error(),
		}
	}
	if res.TimedOut {
		return &recorded, &workflow.Result{
			BlockedCode: "eval_timeout",
			Message:     "eval hook timed out",
		}
	}
	if !res.Passed {
		return &recorded, &workflow.Result{
			BlockedCode: "eval_failed",
			Message:     "eval hook failed",
		}
	}
	return &recorded, nil
}

func (s *Server) handleListTransitions(w http.ResponseWriter, r *http.Request) {
	item, err := s.store.GetWorkItem(r.Context(), r.PathValue("key"))
	if !mapError(w, err) {
		return
	}
	wf, err := s.loadWorkflow(r, item.ProjectKey)
	if !mapError(w, err) {
		return
	}
	type tView struct {
		To                 string             `json:"to"`
		RequireCriteriaMet bool               `json:"require_criteria_met,omitempty"`
		Eval               *workflow.EvalHook `json:"eval,omitempty"`
	}
	var out []tView
	for _, tr := range wf.Available(item.Status) {
		out = append(out, tView{To: tr.To, RequireCriteriaMet: tr.RequireCriteriaMet, Eval: tr.Eval})
	}
	if out == nil {
		out = []tView{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"current":     item.Status,
		"transitions": out,
	})
}

func (s *Server) handleTransition(w http.ResponseWriter, r *http.Request) {
	var req struct {
		To      string `json:"to"`
		Version int64  `json:"version"`
		DryRun  bool   `json:"dry_run,omitempty"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.To) == "" {
		writeError(w, http.StatusBadRequest, "validation_error", "to: required")
		return
	}
	if req.Version < 1 {
		writeError(w, http.StatusBadRequest, "validation_error",
			"version: required (optimistic concurrency)")
		return
	}
	item, err := s.store.GetWorkItem(r.Context(), r.PathValue("key"))
	if !mapError(w, err) {
		return
	}
	if req.Version != item.Version {
		writeError(w, http.StatusConflict, "version_conflict",
			"work item changed since it was read; GET it again and retry")
		return
	}
	wf, err := s.loadWorkflow(r, item.ProjectKey)
	if !mapError(w, err) {
		return
	}
	result := wf.Evaluate(item.Status, req.To, criteriaView(item))
	if !result.Allowed {
		writeBlocked(w, result, nil)
		return
	}
	evalRun, blocked := s.runEvalGate(r, item, result.Transition, req.DryRun)
	if blocked != nil {
		writeBlocked(w, *blocked, evalRun)
		return
	}
	resp := map[string]any{"allowed": true, "transition": result.Transition}
	if evalRun != nil {
		resp["eval_run"] = evalRun
	}
	if req.DryRun {
		writeJSON(w, http.StatusOK, resp)
		return
	}
	item.Status = req.To
	item.Version = req.Version
	updated, err := s.store.UpdateWorkItem(r.Context(), item)
	if !mapError(w, err) {
		return
	}
	resp["work_item"] = updated
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleListEvalRuns(w http.ResponseWriter, r *http.Request) {
	if _, err := s.store.GetWorkItem(r.Context(), r.PathValue("key")); !mapError(w, err) {
		return
	}
	runs, err := s.store.ListEvalRuns(r.Context(), r.PathValue("key"))
	if !mapError(w, err) {
		return
	}
	if runs == nil {
		runs = []store.EvalRun{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"eval_runs": runs})
}

// applyStatusTransition runs the workflow gate for a PATCH-driven status
// change. It evaluates preconditions and eval hooks against the updated
// item, then persists it. Returns the updated item or writes the error.
func (s *Server) applyStatusTransition(w http.ResponseWriter, r *http.Request, item store.WorkItem) (store.WorkItem, bool) {
	orig, err := s.store.GetWorkItem(r.Context(), item.Key)
	if !mapError(w, err) {
		return store.WorkItem{}, false
	}
	// Stale writes lose before any workflow evaluation: a client with an
	// old version has a stale view of the world, so transition advice
	// would be meaningless. (UpdateWorkItem re-checks atomically at write.)
	if item.Version != orig.Version {
		writeError(w, http.StatusConflict, "version_conflict",
			"work item changed since it was read; GET it again and retry")
		return store.WorkItem{}, false
	}
	if item.Status == orig.Status {
		// Not a transition; plain update.
		updated, err := s.store.UpdateWorkItem(r.Context(), item)
		if !mapError(w, err) {
			return store.WorkItem{}, false
		}
		return updated, true
	}
	wf, err := s.loadWorkflow(r, item.ProjectKey)
	if !mapError(w, err) {
		return store.WorkItem{}, false
	}
	// Evaluate against the NEW item: a PATCH may satisfy criteria and
	// transition in one call.
	result := wf.Evaluate(orig.Status, item.Status, criteriaView(item))
	if !result.Allowed {
		writeBlocked(w, result, nil)
		return store.WorkItem{}, false
	}
	evalRun, blocked := s.runEvalGate(r, item, result.Transition, false)
	if blocked != nil {
		writeBlocked(w, *blocked, evalRun)
		return store.WorkItem{}, false
	}
	updated, err := s.store.UpdateWorkItem(r.Context(), item)
	if !mapError(w, err) {
		return store.WorkItem{}, false
	}
	return updated, true
}
