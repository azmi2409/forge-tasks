package store

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"
)

// TestPostgresStore exercises the pgx adapter against a real Postgres.
// It runs in CI (postgres service) and locally with:
//
//	TEST_DATABASE_URL=postgres://forge:forge@localhost:5432/forge_tasks?sslmode=disable go test ./store/ -run TestPostgres
//
// Without TEST_DATABASE_URL the tests skip.
func testPostgresDB(t *testing.T) (*PostgresStore, context.Context) {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping Postgres adapter test")
	}
	ctx := context.Background()
	dbName := fmt.Sprintf("forge_test_%d", os.Getpid())

	// The service database starts empty; migrate it first so the admin
	// connection (which verifies schema presence) succeeds.
	if err := ApplyMigration(ctx, url); err != nil {
		t.Fatalf("migrate service db: %v", err)
	}
	// Create an isolated database per run so parallel CI jobs never clash.
	admin, err := NewPostgresStore(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if _, err := admin.pool.Exec(ctx, `CREATE DATABASE `+dbName); err != nil {
		admin.Close()
		t.Fatalf("create db: %v", err)
	}
	admin.Close()
	t.Cleanup(func() {
		admin, _ := NewPostgresStore(ctx, url)
		if admin != nil {
			admin.pool.Exec(ctx, `DROP DATABASE IF EXISTS `+dbName)
			admin.Close()
		}
	})

	testURL := dbURLWithDB(t, url, dbName)
	if err := ApplyMigration(ctx, testURL); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	s, err := NewPostgresStore(ctx, testURL)
	if err != nil {
		t.Fatalf("connect migrated: %v", err)
	}
	t.Cleanup(s.Close)
	return s, ctx
}

func TestPostgresStore(t *testing.T) {
	s, ctx := testPostgresDB(t)

	if err := s.CreateProject(ctx, Project{Key: "FT", Name: "Forge Tasks"}); err != nil {
		t.Fatalf("create project: %v", err)
	}
	if err := s.CreateProject(ctx, Project{Key: "FT"}); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("duplicate project: got %v, want ErrAlreadyExists", err)
	}

	// Key allocation roundtrip.
	for _, want := range []string{"FT-1", "FT-2"} {
		got, err := s.NextKey(ctx, "FT")
		if err != nil || got != want {
			t.Fatalf("next key: got %q, %v; want %q", got, err, want)
		}
	}

	// Create/get with JSONB fields roundtripping.
	w, err := s.CreateWorkItem(ctx, WorkItem{
		ProjectKey: "FT",
		Title:      "pg roundtrip",
		Acceptance: []AcceptanceCriterion{
			{ID: "a1", Description: "tests pass", Predicate: PredicateTestPasses,
				PredicateArgs: map[string]any{"command": "go test ./..."}, Status: CriterionPending},
		},
		Labels:       []string{"backend"},
		CustomFields: map[string]any{"story_points": float64(3)},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if w.Key != "FT-3" { // counters kept incrementing past the NextKey calls
		t.Fatalf("key: got %q, want FT-3", w.Key)
	}
	got, err := s.GetWorkItem(ctx, w.Key)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got.Acceptance) != 1 || got.Acceptance[0].PredicateArgs["command"] != "go test ./..." {
		t.Fatalf("acceptance JSONB mismatch: %+v", got.Acceptance)
	}
	if got.CustomFields["story_points"] != float64(3) {
		t.Fatalf("custom fields JSONB mismatch: %+v", got.CustomFields)
	}

	// Optimistic concurrency.
	got.Title = "v2"
	updated, err := s.UpdateWorkItem(ctx, got)
	if err != nil || updated.Version != 2 {
		t.Fatalf("update: %v %+v", err, updated)
	}
	got.Title = "stale"
	if _, err := s.UpdateWorkItem(ctx, got); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("stale: got %v, want ErrVersionConflict", err)
	}

	// Concurrent key allocation never collides.
	const n = 50
	var wg sync.WaitGroup
	keys := make(chan string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			k, err := s.NextKey(ctx, "FT")
			if err != nil {
				t.Errorf("next key: %v", err)
				return
			}
			keys <- k
		}()
	}
	wg.Wait()
	close(keys)
	seen := map[string]bool{}
	for k := range keys {
		if seen[k] {
			t.Fatalf("duplicate key: %s", k)
		}
		seen[k] = true
	}

	// Filters incl. the labels @> operator.
	mk := func(title, status string, labels ...string) {
		_, err := s.CreateWorkItem(ctx, WorkItem{ProjectKey: "FT", Title: title, Status: status, Labels: labels})
		if err != nil {
			t.Fatalf("create %s: %v", title, err)
		}
	}
	mk("l1", "todo", "backend", "urgent")
	mk("l2", "todo", "backend")
	res, err := s.ListWorkItems(ctx, WorkItemFilter{Status: "todo", Labels: []string{"backend", "urgent"}})
	if err != nil || len(res) != 1 || res[0].Title != "l1" {
		t.Fatalf("label filter: %v %+v", err, res)
	}

	// Agents.
	if err := s.UpsertAgent(ctx, AgentPrincipal{ID: "a1", Name: "w", Kind: AgentKindAgent}); err != nil {
		t.Fatalf("upsert agent: %v", err)
	}
	if _, err := s.GetAgent(ctx, "a1"); err != nil {
		t.Fatalf("get agent: %v", err)
	}

	// Delete + key never reused.
	if err := s.DeleteWorkItem(ctx, w.Key); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := s.GetWorkItem(ctx, w.Key); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get deleted: got %v, want ErrNotFound", err)
	}
}

func TestPostgresClaims(t *testing.T) {
	s, ctx := testPostgresDB(t)
	if err := s.CreateProject(ctx, Project{Key: "FT", Name: "Forge"}); err != nil {
		t.Fatalf("project: %v", err)
	}
	for _, id := range []string{"agent-1", "agent-2"} {
		if err := s.UpsertAgent(ctx, AgentPrincipal{ID: id, Name: id, Kind: AgentKindAgent}); err != nil {
			t.Fatalf("agent: %v", err)
		}
	}
	if _, err := s.CreateWorkItem(ctx, WorkItem{ProjectKey: "FT", Title: "task"}); err != nil {
		t.Fatalf("create: %v", err)
	}

	w, c, err := s.Claim(ctx, "agent-1", WorkItemFilter{}, time.Minute)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if w.Key != "FT-1" || c.AgentID != "agent-1" {
		t.Fatalf("claim=%+v item=%+v", c, w)
	}
	// Second claim finds nothing: SKIP LOCKED + lease row exclude it.
	if _, _, err := s.Claim(ctx, "agent-2", WorkItemFilter{}, time.Minute); !errors.Is(err, ErrNoWorkAvailable) {
		t.Fatalf("double claim: got %v, want ErrNoWorkAvailable", err)
	}
	// Renew/ownership rules.
	if _, err := s.RenewClaim(ctx, "FT-1", "agent-2", time.Minute); !errors.Is(err, ErrClaimConflict) {
		t.Fatalf("renew wrong agent: got %v, want ErrClaimConflict", err)
	}
	if _, err := s.RenewClaim(ctx, "FT-1", "agent-1", 10*time.Minute); err != nil {
		t.Fatalf("renew: %v", err)
	}
	if err := s.ReleaseClaim(ctx, "FT-1", "agent-1"); err != nil {
		t.Fatalf("release: %v", err)
	}
	if _, _, err := s.Claim(ctx, "agent-2", WorkItemFilter{}, time.Minute); err != nil {
		t.Fatalf("re-claim: %v", err)
	}

	// Concurrent race: exactly one winner across connections.
	if _, err := s.CreateWorkItem(ctx, WorkItem{ProjectKey: "FT", Title: "race"}); err != nil {
		t.Fatalf("create race item: %v", err)
	}
	const n = 16
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins := 0
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			agent := "agent-1"
			if i%2 == 1 {
				agent = "agent-2"
			}
			_, _, err := s.Claim(ctx, agent, WorkItemFilter{}, time.Minute)
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				wins++
			} else if !errors.Is(err, ErrNoWorkAvailable) {
				t.Errorf("unexpected: %v", err)
			}
		}(i)
	}
	wg.Wait()
	// FT-1 is still leased to agent-2; only FT-2 is free, so exactly one
	// of the 16 racers can win.
	if wins != 1 {
		t.Fatalf("race wins=%d, want 1", wins)
	}
}

func TestPostgresWorkflows(t *testing.T) {
	s, ctx := testPostgresDB(t)
	if err := s.CreateProject(ctx, Project{Key: "FT", Name: "Forge"}); err != nil {
		t.Fatalf("project: %v", err)
	}
	// Project auto-pinned to the seeded default workflow.
	pw, err := s.GetProjectWorkflow(ctx, "FT")
	if err != nil {
		t.Fatalf("pin: %v", err)
	}
	if pw.WorkflowID != "default" || pw.Version != 1 {
		t.Fatalf("pw=%+v", pw)
	}
	if _, err := s.GetWorkflow(ctx, "default", 1); err != nil {
		t.Fatalf("default: %v", err)
	}
	if _, err := s.CreateWorkflow(ctx, "bad", badWorkflowDef); err == nil {
		t.Fatal("bad definition: got nil, want error")
	}
	w, err := s.CreateWorkflow(ctx, "Kanban", testWorkflowDef)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	w2, err := s.UpdateWorkflow(ctx, w.ID, testWorkflowDef)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if w2.Version != 2 {
		t.Fatalf("w2=%+v", w2)
	}
	latest, err := s.GetWorkflow(ctx, w.ID, 0)
	if err != nil || latest.Version != 2 {
		t.Fatalf("latest=%+v err=%v", latest, err)
	}
	if err := s.SetProjectWorkflow(ctx, "FT", w.ID, 2); err != nil {
		t.Fatalf("set: %v", err)
	}
	pw, err = s.GetProjectWorkflow(ctx, "FT")
	if err != nil || pw.Version != 2 {
		t.Fatalf("pw=%+v err=%v", pw, err)
	}

	// Eval runs round-trip.
	item, err := s.CreateWorkItem(ctx, WorkItem{ProjectKey: "FT", Title: "task"})
	if err != nil {
		t.Fatalf("create item: %v", err)
	}
	r, err := s.RecordEvalRun(ctx, EvalRun{
		WorkItemKey: item.Key, Transition: "doing->done", HookName: "smoke",
		Command: "true", Passed: true, Output: "ok",
	})
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	if r.ID == "" || r.RanAt.IsZero() {
		t.Fatalf("r=%+v", r)
	}
	runs, err := s.ListEvalRuns(ctx, item.Key)
	if err != nil || len(runs) != 1 || runs[0].ID != r.ID {
		t.Fatalf("runs=%+v err=%v", runs, err)
	}
}

func TestPostgresArtifacts(t *testing.T) {
	s, ctx := testPostgresDB(t)
	if err := s.CreateProject(ctx, Project{Key: "FT", Name: "Forge"}); err != nil {
		t.Fatalf("project: %v", err)
	}
	item, err := s.CreateWorkItem(ctx, WorkItem{ProjectKey: "FT", Title: "task"})
	if err != nil {
		t.Fatalf("item: %v", err)
	}
	a1, err := s.CreateArtifact(ctx, ArtifactInput{
		Kind: ArtifactTestReport, Name: "report.xml", WorkItemKey: item.Key,
		Content:  []byte("<ok/>"),
		Metadata: map[string]any{"status": "pass"},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if a1.Version != 1 || a1.SHA256 == "" {
		t.Fatalf("a1=%+v", a1)
	}
	if _, err := s.CreateArtifact(ctx, ArtifactInput{
		Kind: "bogus", Name: "x", Content: []byte("x"),
	}); err == nil {
		t.Fatal("bad kind: got nil, want error")
	}
	a2, err := s.CreateArtifact(ctx, ArtifactInput{
		Kind: ArtifactTestReport, Name: "report.xml", WorkItemKey: item.Key,
		Content:  []byte("<ok2/>"),
		Metadata: map[string]any{"status": "pass"},
	})
	if err != nil {
		t.Fatalf("create2: %v", err)
	}
	if a2.Version != 2 {
		t.Fatalf("a2=%+v", a2)
	}
	content, err := s.GetArtifactContent(ctx, a1.ID)
	if err != nil || string(content) != "<ok/>" {
		t.Fatalf("content=%q err=%v", content, err)
	}
	listed, err := s.ListArtifacts(ctx, ArtifactFilter{WorkItemKey: item.Key, Kind: ArtifactTestReport})
	if err != nil || len(listed) != 2 {
		t.Fatalf("listed=%+v err=%v", listed, err)
	}
}

func dbURLWithDB(t *testing.T, rawurl, db string) string {
	t.Helper()
	u, err := url.Parse(rawurl)
	if err != nil {
		t.Fatalf("bad TEST_DATABASE_URL: %v", err)
	}
	u.Path = "/" + db
	return u.String()
}
