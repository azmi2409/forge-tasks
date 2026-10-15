package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/azmi2409/forge-tasks/service"
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

// --- transitions (thin adapters over the service layer) ---

// writeBlocked answers "why can't I transition?" in machine-readable form.
func writeBlocked(w http.ResponseWriter, blocked *service.BlockedError) {
	body := map[string]any{
		"code":    blocked.Result.BlockedCode,
		"message": blocked.Result.Message,
	}
	if len(blocked.Result.Missing) > 0 {
		body["missing"] = blocked.Result.Missing
	}
	if len(blocked.Result.Available) > 0 {
		body["available"] = blocked.Result.Available
	}
	if blocked.EvalRun != nil {
		body["eval_run"] = blocked.EvalRun
	}
	writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": body})
}

// mapTransitionError maps service.Transition errors to HTTP responses.
// Returns true when the error was handled.
func (s *Server) mapTransitionError(w http.ResponseWriter, err error) bool {
	var blocked *service.BlockedError
	if errors.As(err, &blocked) {
		writeBlocked(w, blocked)
		return true
	}
	return mapError(w, err)
}

func (s *Server) handleListTransitions(w http.ResponseWriter, r *http.Request) {
	current, transitions, err := s.svc.AvailableTransitions(r.Context(), r.PathValue("key"))
	if !mapError(w, err) {
		return
	}
	type tView struct {
		To                 string             `json:"to"`
		RequireCriteriaMet bool               `json:"require_criteria_met,omitempty"`
		Eval               *workflow.EvalHook `json:"eval,omitempty"`
	}
	out := make([]tView, 0, len(transitions))
	for _, tr := range transitions {
		out = append(out, tView{To: tr.To, RequireCriteriaMet: tr.RequireCriteriaMet, Eval: tr.Eval})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"current":     current,
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
	item.Version = req.Version
	outcome, err := s.svc.Transition(r.Context(), item, req.To, req.DryRun)
	if !s.mapTransitionError(w, err) {
		return
	}
	resp := map[string]any{"allowed": true, "transition": outcome.Transition}
	if outcome.EvalRun != nil {
		resp["eval_run"] = outcome.EvalRun
	}
	if !outcome.DryRun {
		resp["work_item"] = outcome.WorkItem
	}
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
// change. The sparse field updates are applied to the item first, then the
// governed transition runs against that intended state.
func (s *Server) applyStatusTransition(w http.ResponseWriter, r *http.Request, item store.WorkItem, newStatus string) (store.WorkItem, bool) {
	outcome, err := s.svc.Transition(r.Context(), item, newStatus, false)
	if !s.mapTransitionError(w, err) {
		return store.WorkItem{}, false
	}
	return outcome.WorkItem, true
}
