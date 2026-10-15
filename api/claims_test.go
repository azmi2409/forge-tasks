package api

import (
	"net/http"
	"testing"

	"github.com/azmi2409/forge-tasks/store"
)

func seedAgent(t *testing.T, s *Server, id string) {
	t.Helper()
	rec := doRequest(t, s, "PUT", "/api/v1/agents/"+id, map[string]any{
		"name": id, "kind": "agent",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("seed agent: %d", rec.Code)
	}
}

func TestClaimFlow(t *testing.T) {
	s, _ := testServer(t)
	seedProject(t, s)
	seedAgent(t, s, "agent-1")
	seedAgent(t, s, "agent-2")
	doRequest(t, s, "POST", "/api/v1/work-items", map[string]any{
		"project_key": "FT", "title": "task", "labels": []string{"backend"},
	})

	// Claim with a filter.
	rec := doRequest(t, s, "POST", "/api/v1/claims", map[string]any{
		"agent_id":    "agent-1",
		"ttl_seconds": 300,
		"filter":      map[string]any{"labels": []string{"backend"}},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("claim: status=%d body=%s", rec.Code, rec.Body.String())
	}
	var claimed struct {
		Claim    store.Claim    `json:"claim"`
		WorkItem store.WorkItem `json:"work_item"`
	}
	decodeBody(t, rec, &claimed)
	if claimed.Claim.WorkItemKey != "FT-1" || claimed.WorkItem.Status != "todo" {
		t.Fatalf("claimed=%+v", claimed)
	}

	// Queue is now empty for everyone: 204, not an error.
	rec = doRequest(t, s, "POST", "/api/v1/claims", map[string]any{"agent_id": "agent-2"})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("empty queue: status=%d, want 204", rec.Code)
	}

	// Missing agent_id -> 400.
	rec = doRequest(t, s, "POST", "/api/v1/claims", map[string]any{})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("no agent: status=%d, want 400", rec.Code)
	}

	// Get the claim.
	rec = doRequest(t, s, "GET", "/api/v1/claims/FT-1", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get claim: status=%d", rec.Code)
	}

	// Agent's claim list.
	rec = doRequest(t, s, "GET", "/api/v1/agents/agent-1/claims", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list claims: status=%d", rec.Code)
	}
	var list struct {
		Claims []store.Claim `json:"claims"`
	}
	decodeBody(t, rec, &list)
	if len(list.Claims) != 1 {
		t.Fatalf("claims=%+v", list)
	}

	// Renew by the wrong agent -> 409.
	rec = doRequest(t, s, "POST", "/api/v1/claims/FT-1/renew", map[string]any{
		"agent_id": "agent-2",
	})
	if rec.Code != http.StatusConflict {
		t.Fatalf("renew wrong agent: status=%d, want 409", rec.Code)
	}

	// Renew by the holder -> 200.
	rec = doRequest(t, s, "POST", "/api/v1/claims/FT-1/renew", map[string]any{
		"agent_id": "agent-1", "ttl_seconds": 600,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("renew: status=%d body=%s", rec.Code, rec.Body.String())
	}

	// Release by the wrong agent -> 409.
	rec = doRequest(t, s, "DELETE", "/api/v1/claims/FT-1?agent_id=agent-2", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("release wrong agent: status=%d, want 409", rec.Code)
	}

	// Release -> 204, and the item is claimable again.
	rec = doRequest(t, s, "DELETE", "/api/v1/claims/FT-1?agent_id=agent-1", nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("release: status=%d", rec.Code)
	}
	rec = doRequest(t, s, "GET", "/api/v1/claims/FT-1", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("get released: status=%d, want 404", rec.Code)
	}
	rec = doRequest(t, s, "POST", "/api/v1/claims", map[string]any{"agent_id": "agent-2"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("re-claim: status=%d", rec.Code)
	}
}
