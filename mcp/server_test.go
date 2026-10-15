package mcp

import (
	"context"
	"encoding/json"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/azmi2409/forge-tasks/service"
	"github.com/azmi2409/forge-tasks/store"
)

// testSession wires a client to the server over in-memory transports.
func testSession(t *testing.T) *sdk.ClientSession {
	t.Helper()
	ctx := context.Background()
	st := store.NewMemoryStore()
	if err := st.CreateProject(ctx, store.Project{Key: "FT", Name: "Forge"}); err != nil {
		t.Fatalf("project: %v", err)
	}
	server := NewServer(service.New(st, nil))
	client := sdk.NewClient(&sdk.Implementation{Name: "test"}, nil)
	ct, st2 := sdk.NewInMemoryTransports()
	go func() {
		_ = server.Run(ctx, st2)
	}()
	cs, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

func callTool(t *testing.T, cs *sdk.ClientSession, name string, args map[string]any) map[string]any {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{
		Name:      name,
		Arguments: args,
	})
	if err != nil {
		t.Fatalf("call %s: %v", name, err)
	}
	if res.IsError {
		t.Fatalf("call %s: tool error: %+v", name, res.Content)
	}
	out := map[string]any{}
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return out
}

func TestToolList(t *testing.T) {
	cs := testSession(t)
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	want := map[string]bool{
		"list_projects": true, "create_project": true, "create_work_item": true,
		"get_work_item": true, "update_work_item": true, "list_work_items": true,
		"claim_task": true, "renew_claim": true, "release_claim": true,
		"list_transitions": true, "transition": true,
		"upsert_agent": true, "heartbeat": true, "list_eval_runs": true,
		"add_artifact": true, "get_artifact": true, "list_artifacts": true,
	}
	if len(res.Tools) != len(want) {
		names := []string{}
		for _, tl := range res.Tools {
			names = append(names, tl.Name)
		}
		t.Fatalf("got %d tools %v, want %d", len(res.Tools), names, len(want))
	}
	for _, tool := range res.Tools {
		if !want[tool.Name] {
			t.Fatalf("unexpected tool %s", tool.Name)
		}
		delete(want, tool.Name)
		if tool.InputSchema == nil {
			t.Fatalf("tool %s: no input schema", tool.Name)
		}
	}
	if len(want) != 0 {
		t.Fatalf("missing tools: %v", want)
	}
}

// TestAgentLoop drives the whole designed agent flow through MCP tools:
// register -> create -> claim -> transition -> done, plus a blocked
// transition with machine-readable reasons.
func TestAgentLoop(t *testing.T) {
	cs := testSession(t)

	callTool(t, cs, "upsert_agent", map[string]any{"id": "a1", "name": "agent one"})
	callTool(t, cs, "heartbeat", map[string]any{"agent_id": "a1"})

	out := callTool(t, cs, "create_work_item", map[string]any{
		"project_key": "FT",
		"title":       "Do the thing",
		"acceptance": []map[string]any{
			{"id": "c1", "description": "done", "predicate": "manual", "status": "pending"},
		},
	})
	if out["key"] != "FT-1" || out["version"] != float64(1) {
		t.Fatalf("create: %+v", out)
	}

	claim := callTool(t, cs, "claim_task", map[string]any{"agent_id": "a1"})
	if claim["claimed"] != true {
		t.Fatalf("claim: %+v", claim)
	}
	// Queue now empty.
	claim = callTool(t, cs, "claim_task", map[string]any{"agent_id": "a1"})
	if claim["claimed"] != false {
		t.Fatalf("re-claim: %+v", claim)
	}

	tr := callTool(t, cs, "list_transitions", map[string]any{"key": "FT-1"})
	if tr["current"] != "todo" {
		t.Fatalf("transitions: %+v", tr)
	}

	// Blocked: criteria not met -> allowed=false with reasons, NOT an error.
	blocked := callTool(t, cs, "transition", map[string]any{
		"key": "FT-1", "to": "done", "version": 1, "dry_run": true,
	})
	_ = blocked
	// todo->done isn't a transition at all; use the real path instead:
	moved := callTool(t, cs, "transition", map[string]any{"key": "FT-1", "to": "doing", "version": 1})
	if moved["allowed"] != true {
		t.Fatalf("todo->doing: %+v", moved)
	}
	blocked = callTool(t, cs, "transition", map[string]any{"key": "FT-1", "to": "done", "version": 2})
	if blocked["allowed"] != false || blocked["blocked_code"] != "criteria_not_met" {
		t.Fatalf("blocked: %+v", blocked)
	}
	missing, _ := blocked["missing"].([]any)
	if len(missing) != 1 || missing[0] != "c1" {
		t.Fatalf("missing: %+v", blocked)
	}

	// Satisfy the criterion, then go.
	callTool(t, cs, "update_work_item", map[string]any{
		"key": "FT-1", "version": 2,
		"acceptance": []map[string]any{
			{"id": "c1", "description": "done", "predicate": "manual", "status": "passed"},
		},
	})
	done := callTool(t, cs, "transition", map[string]any{"key": "FT-1", "to": "done", "version": 3})
	if done["allowed"] != true {
		t.Fatalf("done: %+v", done)
	}
	got := callTool(t, cs, "get_work_item", map[string]any{"key": "FT-1"})
	if got["status"] != "done" {
		t.Fatalf("status: %+v", got)
	}

	// Release the claim; list shows the work.
	callTool(t, cs, "release_claim", map[string]any{"key": "FT-1", "agent_id": "a1"})
	listed := callTool(t, cs, "list_work_items", map[string]any{"status": "done"})
	items, _ := listed["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("list: %+v", listed)
	}
	projects := callTool(t, cs, "list_projects", map[string]any{})
	if projs, _ := projects["projects"].([]any); len(projs) != 1 {
		t.Fatalf("projects: %+v", projects)
	}
}
