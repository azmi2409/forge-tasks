package api

import (
	"encoding/base64"
	"net/http"
	"strconv"
	"strings"

	"github.com/azmi2409/forge-tasks/store"
)

func (s *Server) handleCreateArtifact(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Kind        string         `json:"kind"`
		Name        string         `json:"name"`
		WorkItemKey string         `json:"work_item_key,omitempty"`
		ContentB64  string         `json:"content_base64"`
		Metadata    map[string]any `json:"metadata,omitempty"`
		CreatedBy   string         `json:"created_by,omitempty"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.ContentB64 == "" {
		writeError(w, http.StatusBadRequest, "validation_error", "content_base64: required")
		return
	}
	content, err := base64.StdEncoding.DecodeString(req.ContentB64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "validation_error", "content_base64: invalid base64")
		return
	}
	a, err := s.store.CreateArtifact(r.Context(), store.ArtifactInput{
		Kind:        req.Kind,
		Name:        req.Name,
		WorkItemKey: req.WorkItemKey,
		Content:     content,
		Metadata:    req.Metadata,
		CreatedBy:   req.CreatedBy,
	})
	if !mapError(w, err) {
		return
	}
	writeJSON(w, http.StatusCreated, a)
}

func (s *Server) handleListArtifacts(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := store.ArtifactFilter{
		WorkItemKey: q.Get("work_item"),
		Kind:        q.Get("kind"),
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
	artifacts, err := s.store.ListArtifacts(r.Context(), f)
	if !mapError(w, err) {
		return
	}
	if artifacts == nil {
		artifacts = []store.Artifact{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"artifacts": artifacts})
}

func (s *Server) handleGetArtifact(w http.ResponseWriter, r *http.Request) {
	a, err := s.store.GetArtifact(r.Context(), r.PathValue("id"))
	if !mapError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, a)
}

func (s *Server) handleGetArtifactContent(w http.ResponseWriter, r *http.Request) {
	a, err := s.store.GetArtifact(r.Context(), r.PathValue("id"))
	if !mapError(w, err) {
		return
	}
	content, err := s.store.GetArtifactContent(r.Context(), r.PathValue("id"))
	if !mapError(w, err) {
		return
	}
	// Content type sniffing: plans/diffs/logs are text; let the client decide.
	w.Header().Set("Content-Type", "application/octet-stream")
	if strings.HasPrefix(a.Kind, "text") || a.Kind == store.ArtifactPlan ||
		a.Kind == store.ArtifactDiff || a.Kind == store.ArtifactLog {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	}
	w.Header().Set("ETag", `"`+a.SHA256+`"`)
	w.Write(content)
}
