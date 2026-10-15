// Package api implements forge-tasks component 2: the REST/JSON API.
//
// The API is a thin translation layer over store.Store: it parses HTTP into
// store calls and maps store errors to HTTP status codes. It is stateless —
// every handler is safe for concurrent use and the server is tuned for high
// connection counts (timeouts on, bounded request bodies).
package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/azmi2409/forge-tasks/store"
)

// maxBodyBytes caps request bodies so one caller can't force large allocs.
const maxBodyBytes = 1 << 20 // 1 MiB

// errorBody is the JSON error envelope.
type errorBody struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// Server is the HTTP API over a store.Store.
type Server struct {
	store  store.Store
	logger *slog.Logger
	mux    *http.ServeMux
}

// NewServer builds the API server. Pass nil logger for a discard logger.
func NewServer(st store.Store, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	s := &Server{store: st, logger: logger, mux: http.NewServeMux()}
	s.routes()
	return s
}

// Handler returns the middleware-wrapped mux.
func (s *Server) Handler() http.Handler {
	return s.withMiddleware(s.mux)
}

// Serve starts the HTTP server with production timeouts. It blocks until ctx
// is cancelled, then shuts down gracefully.
func (s *Server) Serve(ctx context.Context, addr string) error {
	srv := &http.Server{
		Addr:              addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	select {
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutCtx)
	case err := <-errCh:
		return err
	}
}

func (s *Server) routes() {
	m := s.mux
	m.HandleFunc("GET /healthz", s.handleHealth)

	m.HandleFunc("POST /api/v1/projects", s.handleCreateProject)
	m.HandleFunc("GET /api/v1/projects/{key}", s.handleGetProject)
	m.HandleFunc("POST /api/v1/projects/{key}/next-key", s.handleNextKey)

	m.HandleFunc("POST /api/v1/work-items", s.handleCreateWorkItem)
	m.HandleFunc("GET /api/v1/work-items", s.handleListWorkItems)
	m.HandleFunc("GET /api/v1/work-items/{key}", s.handleGetWorkItem)
	m.HandleFunc("PATCH /api/v1/work-items/{key}", s.handlePatchWorkItem)
	m.HandleFunc("DELETE /api/v1/work-items/{key}", s.handleDeleteWorkItem)

	m.HandleFunc("PUT /api/v1/agents/{id}", s.handleUpsertAgent)
	m.HandleFunc("GET /api/v1/agents/{id}", s.handleGetAgent)
	m.HandleFunc("POST /api/v1/agents/{id}/heartbeat", s.handleHeartbeat)

	m.HandleFunc("POST /api/v1/claims", s.handleClaim)
	m.HandleFunc("GET /api/v1/claims/{key}", s.handleGetClaim)
	m.HandleFunc("DELETE /api/v1/claims/{key}", s.handleReleaseClaim)
	m.HandleFunc("POST /api/v1/claims/{key}/renew", s.handleRenewClaim)
	m.HandleFunc("GET /api/v1/agents/{id}/claims", s.handleListClaims)

	m.HandleFunc("POST /api/v1/workflows", s.handleCreateWorkflow)
	m.HandleFunc("GET /api/v1/workflows", s.handleListWorkflows)
	m.HandleFunc("GET /api/v1/workflows/{id}", s.handleGetWorkflow)
	m.HandleFunc("GET /api/v1/workflows/{id}/versions/{version}", s.handleGetWorkflow)
	m.HandleFunc("PUT /api/v1/workflows/{id}", s.handleUpdateWorkflow)
	m.HandleFunc("PUT /api/v1/projects/{key}/workflow", s.handleSetProjectWorkflow)
	m.HandleFunc("GET /api/v1/projects/{key}/workflow", s.handleGetProjectWorkflow)

	m.HandleFunc("GET /api/v1/work-items/{key}/transitions", s.handleListTransitions)
	m.HandleFunc("POST /api/v1/work-items/{key}/transitions", s.handleTransition)
	m.HandleFunc("GET /api/v1/work-items/{key}/evals", s.handleListEvalRuns)
}

// --- middleware ---

func (s *Server) withMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rid := requestID()
		ctx := context.WithValue(r.Context(), requestIDKey{}, rid)
		w.Header().Set("X-Request-ID", rid)
		r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
		start := time.Now()
		rw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rw, r.WithContext(ctx))
		s.logger.Info("request",
			"request_id", rid,
			"method", r.Method,
			"path", r.URL.Path,
			"status", rw.status,
			"duration_ms", time.Since(start).Milliseconds(),
		)
	})
}

type requestIDKey struct{}

func requestID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "unknown"
	}
	return hex.EncodeToString(b[:])
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// --- helpers ---

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	var b errorBody
	b.Error.Code = code
	b.Error.Message = message
	writeJSON(w, status, b)
}

// mapError converts store sentinel errors to HTTP responses. ok=false means
// the error was written; the handler should return.
func mapError(w http.ResponseWriter, err error) bool {
	if err == nil {
		return true
	}
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, store.ErrAlreadyExists):
		writeError(w, http.StatusConflict, "already_exists", err.Error())
	case errors.Is(err, store.ErrVersionConflict):
		writeError(w, http.StatusConflict, "version_conflict",
			"work item changed since it was read; GET it again and retry")
	case errors.Is(err, store.ErrClaimConflict):
		writeError(w, http.StatusConflict, "claim_conflict", err.Error())
	default:
		var ve *store.ValidationError
		if errors.As(err, &ve) {
			writeError(w, http.StatusBadRequest, "validation_error", ve.Error())
		} else if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			writeError(w, http.StatusServiceUnavailable, "cancelled", "request cancelled")
		} else {
			writeError(w, http.StatusInternalServerError, "internal", "internal error")
		}
	}
	return false
}

func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON: "+err.Error())
		return false
	}
	return true
}

// --- handlers ---

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleCreateProject(w http.ResponseWriter, r *http.Request) {
	var p store.Project
	if !decodeJSON(w, r, &p) {
		return
	}
	if err := s.store.CreateProject(r.Context(), p); !mapError(w, err) {
		return
	}
	writeJSON(w, http.StatusCreated, p)
}

func (s *Server) handleGetProject(w http.ResponseWriter, r *http.Request) {
	p, err := s.store.GetProject(r.Context(), r.PathValue("key"))
	if !mapError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) handleNextKey(w http.ResponseWriter, r *http.Request) {
	key, err := s.store.NextKey(r.Context(), r.PathValue("key"))
	if !mapError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"key": key})
}

func (s *Server) handleCreateWorkItem(w http.ResponseWriter, r *http.Request) {
	var item store.WorkItem
	if !decodeJSON(w, r, &item) {
		return
	}
	created, err := s.store.CreateWorkItem(r.Context(), item)
	if !mapError(w, err) {
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

func (s *Server) handleGetWorkItem(w http.ResponseWriter, r *http.Request) {
	item, err := s.store.GetWorkItem(r.Context(), r.PathValue("key"))
	if !mapError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, item)
}

// workItemPatch applies sparse updates; version is required for the
// optimistic-concurrency check.
type workItemPatch struct {
	Version      int64                        `json:"version"`
	Title        *string                      `json:"title,omitempty"`
	Description  *string                      `json:"description,omitempty"`
	IssueType    *string                      `json:"issue_type,omitempty"`
	Status       *string                      `json:"status,omitempty"`
	Priority     *string                      `json:"priority,omitempty"`
	AssigneeID   *string                      `json:"assignee_id,omitempty"`
	Acceptance   *[]store.AcceptanceCriterion `json:"acceptance,omitempty"`
	Labels       *[]string                    `json:"labels,omitempty"`
	CustomFields *map[string]any              `json:"custom_fields,omitempty"`
}

func (s *Server) handlePatchWorkItem(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	var patch workItemPatch
	if !decodeJSON(w, r, &patch) {
		return
	}
	if patch.Version <= 0 {
		writeError(w, http.StatusBadRequest, "validation_error", "version: required and must be positive")
		return
	}
	cur, err := s.store.GetWorkItem(r.Context(), key)
	if !mapError(w, err) {
		return
	}
	cur.Version = patch.Version
	if patch.Title != nil {
		cur.Title = *patch.Title
	}
	if patch.Description != nil {
		cur.Description = *patch.Description
	}
	if patch.IssueType != nil {
		cur.IssueType = *patch.IssueType
	}
	if patch.Status != nil {
		cur.Status = *patch.Status
	}
	if patch.Priority != nil {
		cur.Priority = *patch.Priority
	}
	if patch.AssigneeID != nil {
		cur.AssigneeID = *patch.AssigneeID
	}
	if patch.Acceptance != nil {
		cur.Acceptance = *patch.Acceptance
	}
	if patch.Labels != nil {
		cur.Labels = *patch.Labels
	}
	if patch.CustomFields != nil {
		cur.CustomFields = *patch.CustomFields
	}
	// Status changes always go through the workflow engine: preconditions
	// are checked and eval hooks run. This is the single choke point, so
	// the workflow can't be bypassed with a raw PATCH.
	updated, ok := s.applyStatusTransition(w, r, cur)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) handleDeleteWorkItem(w http.ResponseWriter, r *http.Request) {
	if err := s.store.DeleteWorkItem(r.Context(), r.PathValue("key")); !mapError(w, err) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleListWorkItems(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := store.WorkItemFilter{
		ProjectKey: q.Get("project"),
		Status:     q.Get("status"),
		AssigneeID: q.Get("assignee"),
		IssueType:  q.Get("issue_type"),
	}
	if labels := q.Get("labels"); labels != "" {
		f.Labels = strings.Split(labels, ",")
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, "bad_request", "limit must be a non-negative integer")
			return
		}
		f.Limit = n
	}
	if v := q.Get("offset"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, "bad_request", "offset must be a non-negative integer")
			return
		}
		f.Offset = n
	}
	items, err := s.store.ListWorkItems(r.Context(), f)
	if !mapError(w, err) {
		return
	}
	if items == nil {
		items = []store.WorkItem{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) handleUpsertAgent(w http.ResponseWriter, r *http.Request) {
	var a store.AgentPrincipal
	if !decodeJSON(w, r, &a) {
		return
	}
	a.ID = r.PathValue("id") // path wins; one canonical identity
	if err := s.store.UpsertAgent(r.Context(), a); !mapError(w, err) {
		return
	}
	a.ID = r.PathValue("id")
	writeJSON(w, http.StatusOK, a)
}

func (s *Server) handleGetAgent(w http.ResponseWriter, r *http.Request) {
	a, err := s.store.GetAgent(r.Context(), r.PathValue("id"))
	if !mapError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, a)
}

// --- claims ---

// claimRequest asks for one work item lease.
type claimRequest struct {
	AgentID    string       `json:"agent_id"`
	TTLSeconds *int64       `json:"ttl_seconds,omitempty"` // default DefaultClaimTTL
	Filter     *claimFilter `json:"filter,omitempty"`
}

type claimFilter struct {
	Project   string   `json:"project,omitempty"`
	Status    string   `json:"status,omitempty"`
	IssueType string   `json:"issue_type,omitempty"`
	Labels    []string `json:"labels,omitempty"`
}

func (s *Server) handleClaim(w http.ResponseWriter, r *http.Request) {
	var req claimRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.AgentID) == "" {
		writeError(w, http.StatusBadRequest, "validation_error", "agent_id: required")
		return
	}
	ttl := store.DefaultClaimTTL
	if req.TTLSeconds != nil {
		ttl = time.Duration(*req.TTLSeconds) * time.Second
	}
	var f store.WorkItemFilter
	if req.Filter != nil {
		f = store.WorkItemFilter{
			ProjectKey: req.Filter.Project,
			Status:     req.Filter.Status,
			IssueType:  req.Filter.IssueType,
			Labels:     req.Filter.Labels,
		}
	}
	item, claim, err := s.store.Claim(r.Context(), req.AgentID, f, ttl)
	if errors.Is(err, store.ErrNoWorkAvailable) {
		// Empty queue is normal for poll loops, not an error.
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if !mapError(w, err) {
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"claim":     claim,
		"work_item": item,
	})
}

func (s *Server) handleGetClaim(w http.ResponseWriter, r *http.Request) {
	c, err := s.store.GetClaim(r.Context(), r.PathValue("key"))
	if !mapError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (s *Server) handleReleaseClaim(w http.ResponseWriter, r *http.Request) {
	agentID := r.URL.Query().Get("agent_id")
	if strings.TrimSpace(agentID) == "" {
		writeError(w, http.StatusBadRequest, "validation_error", "agent_id query param: required")
		return
	}
	if err := s.store.ReleaseClaim(r.Context(), r.PathValue("key"), agentID); !mapError(w, err) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleRenewClaim(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AgentID    string `json:"agent_id"`
		TTLSeconds *int64 `json:"ttl_seconds,omitempty"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.AgentID) == "" {
		writeError(w, http.StatusBadRequest, "validation_error", "agent_id: required")
		return
	}
	ttl := store.DefaultClaimTTL
	if req.TTLSeconds != nil {
		ttl = time.Duration(*req.TTLSeconds) * time.Second
	}
	c, err := s.store.RenewClaim(r.Context(), r.PathValue("key"), req.AgentID, ttl)
	if !mapError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (s *Server) handleListClaims(w http.ResponseWriter, r *http.Request) {
	claims, err := s.store.ListClaims(r.Context(), r.PathValue("id"))
	if !mapError(w, err) {
		return
	}
	if claims == nil {
		claims = []store.Claim{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"claims": claims})
}

func (s *Server) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct {
		At *time.Time `json:"at,omitempty"`
	}
	// Heartbeat body is optional; empty body means "now".
	if r.ContentLength != 0 {
		if !decodeJSON(w, r, &body) {
			return
		}
	}
	at := time.Now().UTC()
	if body.At != nil {
		at = body.At.UTC()
	}
	if err := s.store.Heartbeat(r.Context(), id, at); !mapError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "last_heartbeat": at.Format(time.RFC3339)})
}
