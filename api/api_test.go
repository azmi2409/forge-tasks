package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/azmi2409/forge-tasks/store"
)

func testServer(t *testing.T) (*Server, *store.MemoryStore) {
	t.Helper()
	st := store.NewMemoryStore()
	return NewServer(st, nil), st
}

func doRequest(t *testing.T, s *Server, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode: %v", err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func decodeBody(t *testing.T, rec *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.NewDecoder(rec.Body).Decode(v); err != nil {
		t.Fatalf("decode response: %v (body: %s)", err, rec.Body.String())
	}
}

func TestHealth(t *testing.T) {
	s, _ := testServer(t)
	rec := doRequest(t, s, "GET", "/healthz", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	if rec.Header().Get("X-Request-ID") == "" {
		t.Fatal("missing X-Request-ID")
	}
}

func TestProjects(t *testing.T) {
	s, _ := testServer(t)

	rec := doRequest(t, s, "POST", "/api/v1/projects", map[string]string{"key": "FT", "name": "Forge"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: status=%d body=%s", rec.Code, rec.Body.String())
	}
	rec = doRequest(t, s, "POST", "/api/v1/projects", map[string]string{"key": "FT", "name": "Dup"})
	if rec.Code != http.StatusConflict {
		t.Fatalf("duplicate: status=%d, want 409", rec.Code)
	}
	rec = doRequest(t, s, "POST", "/api/v1/projects", map[string]string{"key": "bad key"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid: status=%d, want 400", rec.Code)
	}

	rec = doRequest(t, s, "GET", "/api/v1/projects/FT", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get: status=%d", rec.Code)
	}
	var p store.Project
	decodeBody(t, rec, &p)
	if p.Name != "Forge" {
		t.Fatalf("project=%+v", p)
	}
	rec = doRequest(t, s, "GET", "/api/v1/projects/NOPE", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing: status=%d, want 404", rec.Code)
	}

	rec = doRequest(t, s, "POST", "/api/v1/projects/FT/next-key", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("next-key: status=%d", rec.Code)
	}
	var kr map[string]string
	decodeBody(t, rec, &kr)
	if kr["key"] != "FT-1" {
		t.Fatalf("key=%v", kr)
	}
}

func seedProject(t *testing.T, s *Server) {
	t.Helper()
	rec := doRequest(t, s, "POST", "/api/v1/projects", map[string]string{"key": "FT", "name": "Forge"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("seed project: %d", rec.Code)
	}
}

func TestWorkItemCRUD(t *testing.T) {
	s, _ := testServer(t)
	seedProject(t, s)

	// Create (key auto-allocated).
	rec := doRequest(t, s, "POST", "/api/v1/work-items", map[string]any{
		"project_key": "FT",
		"title":       "Build the API",
		"acceptance": []map[string]any{
			{"id": "a1", "description": "tests green", "predicate": "test_passes", "status": "pending"},
		},
		"labels": []string{"backend"},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: status=%d body=%s", rec.Code, rec.Body.String())
	}
	var created store.WorkItem
	decodeBody(t, rec, &created)
	if created.Key != "FT-1" || created.Version != 1 || created.Status != "todo" {
		t.Fatalf("created=%+v", created)
	}

	// Create invalid.
	rec = doRequest(t, s, "POST", "/api/v1/work-items", map[string]any{"project_key": "FT"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid: status=%d, want 400", rec.Code)
	}

	// Get.
	rec = doRequest(t, s, "GET", "/api/v1/work-items/FT-1", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get: status=%d", rec.Code)
	}

	// Patch with correct version.
	rec = doRequest(t, s, "PATCH", "/api/v1/work-items/FT-1", map[string]any{
		"version": 1,
		"status":  "doing",
		"title":   "Build the API (v2)",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("patch: status=%d body=%s", rec.Code, rec.Body.String())
	}
	var patched store.WorkItem
	decodeBody(t, rec, &patched)
	if patched.Version != 2 || patched.Status != "doing" || patched.Title != "Build the API (v2)" {
		t.Fatalf("patched=%+v", patched)
	}
	if len(patched.Acceptance) != 1 || len(patched.Labels) != 1 {
		t.Fatalf("patch dropped fields: %+v", patched)
	}

	// Patch with stale version -> 409.
	rec = doRequest(t, s, "PATCH", "/api/v1/work-items/FT-1", map[string]any{
		"version": 1,
		"status":  "done",
	})
	if rec.Code != http.StatusConflict {
		t.Fatalf("stale patch: status=%d, want 409", rec.Code)
	}
	var eb map[string]map[string]string
	decodeBody(t, rec, &eb)
	if eb["error"]["code"] != "version_conflict" {
		t.Fatalf("error code=%v", eb)
	}

	// Patch without version -> 400.
	rec = doRequest(t, s, "PATCH", "/api/v1/work-items/FT-1", map[string]any{"status": "done"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("no version: status=%d, want 400", rec.Code)
	}

	// Delete.
	rec = doRequest(t, s, "DELETE", "/api/v1/work-items/FT-1", nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete: status=%d", rec.Code)
	}
	rec = doRequest(t, s, "GET", "/api/v1/work-items/FT-1", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("get deleted: status=%d, want 404", rec.Code)
	}
}

func TestListWorkItems(t *testing.T) {
	s, _ := testServer(t)
	seedProject(t, s)
	mk := func(title, status string, labels []string) {
		rec := doRequest(t, s, "POST", "/api/v1/work-items", map[string]any{
			"project_key": "FT", "title": title, "status": status, "labels": labels,
		})
		if rec.Code != http.StatusCreated {
			t.Fatalf("seed %s: %d", title, rec.Code)
		}
	}
	mk("a", "todo", []string{"backend"})
	mk("b", "doing", []string{"backend", "urgent"})
	mk("c", "todo", []string{"frontend"})

	list := func(query string) []store.WorkItem {
		t.Helper()
		rec := doRequest(t, s, "GET", "/api/v1/work-items"+query, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("list %s: status=%d", query, rec.Code)
		}
		var out struct {
			Items []store.WorkItem `json:"items"`
		}
		decodeBody(t, rec, &out)
		return out.Items
	}

	if got := list(""); len(got) != 3 {
		t.Fatalf("all: got %d", len(got))
	}
	if got := list("?status=todo"); len(got) != 2 {
		t.Fatalf("status: got %d", len(got))
	}
	if got := list("?labels=backend,urgent"); len(got) != 1 || got[0].Title != "b" {
		t.Fatalf("labels: %+v", got)
	}
	if got := list("?limit=1&offset=1"); len(got) != 1 || got[0].Key != "FT-2" {
		t.Fatalf("page: %+v", got)
	}
	rec := doRequest(t, s, "GET", "/api/v1/work-items?limit=nope", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad limit: status=%d, want 400", rec.Code)
	}
}

func TestAgents(t *testing.T) {
	s, _ := testServer(t)

	rec := doRequest(t, s, "PUT", "/api/v1/agents/agent-1", map[string]any{
		"name": "worker", "kind": "agent", "capabilities": []string{"code"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("upsert: status=%d body=%s", rec.Code, rec.Body.String())
	}
	rec = doRequest(t, s, "GET", "/api/v1/agents/agent-1", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get: status=%d", rec.Code)
	}
	var a store.AgentPrincipal
	decodeBody(t, rec, &a)
	if a.ID != "agent-1" || a.Kind != store.AgentKindAgent {
		t.Fatalf("agent=%+v", a)
	}
	rec = doRequest(t, s, "POST", "/api/v1/agents/agent-1/heartbeat", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("heartbeat: status=%d", rec.Code)
	}
	rec = doRequest(t, s, "POST", "/api/v1/agents/ghost/heartbeat", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("ghost heartbeat: status=%d, want 404", rec.Code)
	}
}

func TestBadJSON(t *testing.T) {
	s, _ := testServer(t)
	req := httptest.NewRequest("POST", "/api/v1/projects", strings.NewReader("{not json"))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400", rec.Code)
	}
}
