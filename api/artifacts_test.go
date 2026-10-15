package api

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/azmi2409/forge-tasks/store"
)

func TestArtifactEndpoints(t *testing.T) {
	s, _ := testServer(t)
	seedProject(t, s)
	doRequest(t, s, "POST", "/api/v1/work-items", map[string]any{
		"project_key": "FT", "title": "task",
	})

	b64 := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

	// Create.
	rec := doRequest(t, s, "POST", "/api/v1/artifacts", map[string]any{
		"kind": "test_report", "name": "report.xml", "work_item_key": "FT-1",
		"content_base64": b64("<ok/>"), "metadata": map[string]any{"status": "pass"},
		"created_by": "a1",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	var a store.Artifact
	decodeBody(t, rec, &a)
	if a.Version != 1 || a.Size != 5 {
		t.Fatalf("a=%+v", a)
	}

	// Bad kind -> 400.
	rec = doRequest(t, s, "POST", "/api/v1/artifacts", map[string]any{
		"kind": "bogus", "name": "x", "content_base64": b64("x"),
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad kind: %d, want 400", rec.Code)
	}

	// Bad base64 -> 400.
	rec = doRequest(t, s, "POST", "/api/v1/artifacts", map[string]any{
		"kind": "plan", "name": "x", "content_base64": "!!!",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad b64: %d, want 400", rec.Code)
	}

	// Get metadata + content.
	rec = doRequest(t, s, "GET", "/api/v1/artifacts/"+a.ID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get: %d", rec.Code)
	}
	rec = doRequest(t, s, "GET", "/api/v1/artifacts/"+a.ID+"/content", nil)
	if rec.Code != http.StatusOK || rec.Body.String() != "<ok/>" {
		t.Fatalf("content: %d %q", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("ETag") == "" {
		t.Fatal("missing ETag")
	}

	// List filtered.
	rec = doRequest(t, s, "GET", "/api/v1/artifacts?work_item=FT-1&kind=test_report", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d", rec.Code)
	}
	var listed struct {
		Artifacts []store.Artifact `json:"artifacts"`
	}
	decodeBody(t, rec, &listed)
	if len(listed.Artifacts) != 1 {
		t.Fatalf("listed=%+v", listed)
	}

	// Unknown id -> 404.
	rec = doRequest(t, s, "GET", "/api/v1/artifacts/art-99", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing: %d, want 404", rec.Code)
	}
}

func TestEvalProducesArtifact(t *testing.T) {
	s, _ := testServer(t)
	seedProject(t, s)

	// Workflow whose doing->done eval always passes with output.
	def := `{
		"states": [{"name": "todo", "initial": true}, {"name": "doing"}, {"name": "done", "terminal": true}],
		"transitions": [
			{"from": "todo", "to": "doing"},
			{"from": "doing", "to": "done",
			 "eval": {"name": "smoke", "command": "echo hello"}}
		]
	}`
	rec := doRequest(t, s, "POST", "/api/v1/workflows", map[string]any{
		"name": "gated", "definition": json.RawMessage(def),
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("wf: %d %s", rec.Code, rec.Body.String())
	}
	var wf store.StoredWorkflow
	decodeBody(t, rec, &wf)
	rec = doRequest(t, s, "PUT", "/api/v1/projects/FT/workflow",
		map[string]any{"workflow_id": wf.ID, "version": 1})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("pin: %d", rec.Code)
	}

	doRequest(t, s, "POST", "/api/v1/work-items", map[string]any{"project_key": "FT", "title": "t"})
	doRequest(t, s, "POST", "/api/v1/work-items/FT-1/transitions",
		map[string]any{"to": "doing", "version": 1})
	rec = doRequest(t, s, "POST", "/api/v1/work-items/FT-1/transitions",
		map[string]any{"to": "done", "version": 2})
	if rec.Code != http.StatusOK {
		t.Fatalf("transition: %d %s", rec.Code, rec.Body.String())
	}

	// The eval output was mirrored as an eval_result artifact.
	rec = doRequest(t, s, "GET", "/api/v1/artifacts?work_item=FT-1&kind=eval_result", nil)
	var listed struct {
		Artifacts []store.Artifact `json:"artifacts"`
	}
	decodeBody(t, rec, &listed)
	if len(listed.Artifacts) != 1 {
		t.Fatalf("artifacts=%+v", listed)
	}
	art := listed.Artifacts[0]
	if art.Metadata["passed"] != true || art.Name != "eval-smoke" {
		t.Fatalf("art=%+v", art)
	}
	rec = doRequest(t, s, "GET", "/api/v1/artifacts/"+art.ID+"/content", nil)
	if rec.Body.String() != "hello\n" {
		t.Fatalf("content=%q", rec.Body.String())
	}
}
