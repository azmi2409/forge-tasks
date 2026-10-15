package store

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

func testStore(t *testing.T) *MemoryStore {
	t.Helper()
	s := NewMemoryStore()
	ctx := context.Background()
	if err := s.CreateProject(ctx, Project{Key: "FT", Name: "Forge Tasks"}); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	return s
}

func TestCreateProject(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()

	if err := s.CreateProject(ctx, Project{Key: "FT", Name: "Forge Tasks"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.CreateProject(ctx, Project{Key: "FT", Name: "Dup"}); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("duplicate: got %v, want ErrAlreadyExists", err)
	}
	if err := s.CreateProject(ctx, Project{Key: "lower", Name: "Bad"}); err == nil {
		t.Fatal("lowercase key: got nil, want validation error")
	}
	p, err := s.GetProject(ctx, "FT")
	if err != nil || p.Name != "Forge Tasks" {
		t.Fatalf("get: %v %+v", err, p)
	}
	if _, err := s.GetProject(ctx, "NOPE"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: got %v, want ErrNotFound", err)
	}
}

func TestNextKeySequence(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	for i, want := range []string{"FT-1", "FT-2", "FT-3"} {
		got, err := s.NextKey(ctx, "FT")
		if err != nil || got != want {
			t.Fatalf("key %d: got %q, %v; want %q", i, got, err, want)
		}
	}
	if _, err := s.NextKey(ctx, "NOPE"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown project: got %v, want ErrNotFound", err)
	}
}

func TestCreateGetWorkItem(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	now := time.Now().UTC()
	w, err := s.CreateWorkItem(ctx, WorkItem{
		ProjectKey: "FT",
		Title:      "Implement claim API",
		Acceptance: []AcceptanceCriterion{
			{ID: "a1", Description: "race for the same item", Predicate: PredicateTestPasses,
				PredicateArgs: map[string]any{"command": "go test -race ./store/"}, Status: CriterionPending},
		},
		Labels:       []string{"backend", "concurrency"},
		CustomFields: map[string]any{"story_points": 3},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if w.Key != "FT-1" {
		t.Fatalf("key: got %q, want FT-1", w.Key)
	}
	if w.Version != 1 || w.Status != DefaultStatus || w.IssueType != DefaultIssueType || w.Priority != DefaultPriority {
		t.Fatalf("defaults not applied: %+v", w)
	}
	if w.CreatedAt.Before(now.Add(-time.Minute)) {
		t.Fatalf("created_at not set: %v", w.CreatedAt)
	}

	got, err := s.GetWorkItem(ctx, "FT-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Title != "Implement claim API" || len(got.Acceptance) != 1 || got.Acceptance[0].Predicate != PredicateTestPasses {
		t.Fatalf("roundtrip mismatch: %+v", got)
	}
	if _, err := s.GetWorkItem(ctx, "FT-999"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: got %v, want ErrNotFound", err)
	}
}

func TestCreateWorkItemValidation(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	if _, err := s.CreateWorkItem(ctx, WorkItem{ProjectKey: "FT"}); err == nil {
		t.Fatal("empty title: got nil, want validation error")
	}
	if _, err := s.CreateWorkItem(ctx, WorkItem{ProjectKey: "NOPE", Title: "x"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown project: got %v, want ErrNotFound", err)
	}
	if _, err := s.CreateWorkItem(ctx, WorkItem{
		ProjectKey: "FT", Title: "x",
		Acceptance: []AcceptanceCriterion{{Description: "bad", Predicate: "nope"}},
	}); err == nil {
		t.Fatal("bad predicate: got nil, want validation error")
	}
	if _, err := s.CreateWorkItem(ctx, WorkItem{ProjectKey: "FT", Key: "XX-1", Title: "x"}); err == nil {
		t.Fatal("key prefix mismatch: got nil, want validation error")
	}
	if _, err := s.CreateWorkItem(ctx, WorkItem{ProjectKey: "FT", Key: "FT-1", Title: "a"}); err != nil {
		t.Fatalf("explicit key: %v", err)
	}
	if _, err := s.CreateWorkItem(ctx, WorkItem{ProjectKey: "FT", Key: "FT-1", Title: "b"}); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("duplicate key: got %v, want ErrAlreadyExists", err)
	}
}

func TestUpdateWorkItem(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	w, err := s.CreateWorkItem(ctx, WorkItem{ProjectKey: "FT", Title: "v1"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	w.Title = "v2"
	updated, err := s.UpdateWorkItem(ctx, w)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.Version != 2 || updated.Title != "v2" {
		t.Fatalf("bad update: %+v", updated)
	}
	if !updated.UpdatedAt.After(w.CreatedAt) && updated.UpdatedAt.Before(w.CreatedAt.Add(-time.Second)) {
		t.Fatalf("updated_at not refreshed")
	}

	// Stale write must fail.
	w.Title = "stale"
	if _, err := s.UpdateWorkItem(ctx, w); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("stale update: got %v, want ErrVersionConflict", err)
	}
	// Missing item.
	w.Key = "FT-999"
	if _, err := s.UpdateWorkItem(ctx, w); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing update: got %v, want ErrNotFound", err)
	}
}

// TestConcurrentUpdates hammers one item from many goroutines: exactly one
// writer wins per version; the rest get ErrVersionConflict.
func TestConcurrentUpdates(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	w, err := s.CreateWorkItem(ctx, WorkItem{ProjectKey: "FT", Title: "race"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	const n = 32
	// Every writer reads the same version up front; then all are released
	// at once, so exactly one stale write can win.
	reads := make([]WorkItem, n)
	for i := 0; i < n; i++ {
		cur, err := s.GetWorkItem(ctx, w.Key)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		reads[i] = cur
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins, conflicts := 0, 0
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			cur := reads[i]
			cur.Title = fmt.Sprintf("writer-%d", i)
			_, err := s.UpdateWorkItem(ctx, cur)
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				wins++
			} else if errors.Is(err, ErrVersionConflict) {
				conflicts++
			} else {
				t.Errorf("unexpected error: %v", err)
			}
		}(i)
	}
	close(start)
	wg.Wait()
	if wins != 1 || conflicts != n-1 {
		t.Fatalf("wins=%d conflicts=%d, want wins=1 conflicts=%d", wins, conflicts, n-1)
	}
	final, _ := s.GetWorkItem(ctx, w.Key)
	if final.Version != 2 {
		t.Fatalf("final version=%d, want 2", final.Version)
	}
}

// TestConcurrentCreates proves key allocation never collides under load.
func TestConcurrentCreates(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	const n = 100
	var wg sync.WaitGroup
	keys := make(chan string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			w, err := s.CreateWorkItem(ctx, WorkItem{ProjectKey: "FT", Title: fmt.Sprintf("task-%d", i)})
			if err != nil {
				t.Errorf("create: %v", err)
				return
			}
			keys <- w.Key
		}(i)
	}
	wg.Wait()
	close(keys)
	seen := map[string]bool{}
	for k := range keys {
		if seen[k] {
			t.Fatalf("duplicate key allocated: %s", k)
		}
		seen[k] = true
	}
	if len(seen) != n {
		t.Fatalf("got %d unique keys, want %d", len(seen), n)
	}
}

func TestListWorkItems(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	mk := func(title, status, assignee string, labels ...string) {
		w := WorkItem{ProjectKey: "FT", Title: title, Status: status, AssigneeID: assignee, Labels: labels}
		if _, err := s.CreateWorkItem(ctx, w); err != nil {
			t.Fatalf("create %s: %v", title, err)
		}
	}
	mk("a", "todo", "agent-1", "backend")
	mk("b", "doing", "agent-1", "backend", "urgent")
	mk("c", "todo", "agent-2", "frontend")
	mk("d", "done", "", "backend")

	all, err := s.ListWorkItems(ctx, WorkItemFilter{})
	if err != nil || len(all) != 4 {
		t.Fatalf("list all: %v %d", err, len(all))
	}
	// Deterministic key order.
	for i := 1; i < len(all); i++ {
		if all[i-1].Key >= all[i].Key {
			t.Fatalf("not key-ordered: %v", all)
		}
	}

	todos, _ := s.ListWorkItems(ctx, WorkItemFilter{Status: "todo"})
	if len(todos) != 2 {
		t.Fatalf("status filter: got %d, want 2", len(todos))
	}
	mine, _ := s.ListWorkItems(ctx, WorkItemFilter{AssigneeID: "agent-1"})
	if len(mine) != 2 {
		t.Fatalf("assignee filter: got %d, want 2", len(mine))
	}
	both, _ := s.ListWorkItems(ctx, WorkItemFilter{Labels: []string{"backend", "urgent"}})
	if len(both) != 1 || both[0].Title != "b" {
		t.Fatalf("label AND filter: %+v", both)
	}
	paged, _ := s.ListWorkItems(ctx, WorkItemFilter{Limit: 2, Offset: 1})
	if len(paged) != 2 || paged[0].Key != "FT-2" {
		t.Fatalf("pagination: %+v", paged)
	}
	over, _ := s.ListWorkItems(ctx, WorkItemFilter{Limit: MaxListLimit + 5000})
	if len(over) != 4 {
		t.Fatalf("limit cap: got %d, want 4", len(over))
	}
}

// TestDeepCopyIsolation ensures callers can't mutate stored state through
// returned values.
func TestDeepCopyIsolation(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	w, _ := s.CreateWorkItem(ctx, WorkItem{
		ProjectKey: "FT", Title: "iso", Labels: []string{"a"},
		Acceptance: []AcceptanceCriterion{{ID: "c1", Description: "d", Predicate: PredicateManual, Status: CriterionPending}},
	})
	w.Labels[0] = "MUTATED"
	w.Acceptance[0].Status = CriterionPassed
	fresh, _ := s.GetWorkItem(ctx, w.Key)
	if fresh.Labels[0] != "a" || fresh.Acceptance[0].Status != CriterionPending {
		t.Fatalf("stored state mutated via returned value: %+v", fresh)
	}
}

func TestDeleteWorkItem(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	w, _ := s.CreateWorkItem(ctx, WorkItem{ProjectKey: "FT", Title: "bye"})
	if err := s.DeleteWorkItem(ctx, w.Key); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := s.GetWorkItem(ctx, w.Key); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get after delete: got %v, want ErrNotFound", err)
	}
	if err := s.DeleteWorkItem(ctx, w.Key); !errors.Is(err, ErrNotFound) {
		t.Fatalf("double delete: got %v, want ErrNotFound", err)
	}
	// Keys are never reused: the next item is FT-2, not FT-1.
	next, _ := s.CreateWorkItem(ctx, WorkItem{ProjectKey: "FT", Title: "next"})
	if next.Key != "FT-2" {
		t.Fatalf("key reuse: got %q, want FT-2", next.Key)
	}
}

func TestAgents(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	hb := time.Now().UTC().Add(-time.Minute)
	if err := s.UpsertAgent(ctx, AgentPrincipal{ID: "agent-1", Name: "worker", Kind: AgentKindAgent, LastHeartbeat: hb}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := s.UpsertAgent(ctx, AgentPrincipal{ID: "bad", Kind: "robot"}); err == nil {
		t.Fatal("bad kind: got nil, want validation error")
	}
	a, err := s.GetAgent(ctx, "agent-1")
	if err != nil || a.Name != "worker" {
		t.Fatalf("get: %v %+v", err, a)
	}
	if _, err := s.GetAgent(ctx, "ghost"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: got %v, want ErrNotFound", err)
	}
	later := hb.Add(time.Hour)
	if err := s.Heartbeat(ctx, "agent-1", later); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	a, _ = s.GetAgent(ctx, "agent-1")
	if !a.LastHeartbeat.Equal(later) {
		t.Fatalf("heartbeat not recorded: %v", a.LastHeartbeat)
	}
	if err := s.Heartbeat(ctx, "ghost", later); !errors.Is(err, ErrNotFound) {
		t.Fatalf("heartbeat missing: got %v, want ErrNotFound", err)
	}
}

func TestContextCancellation(t *testing.T) {
	s := testStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.CreateProject(ctx, Project{Key: "XX"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled create: got %v, want context.Canceled", err)
	}
	if _, err := s.GetWorkItem(ctx, "FT-1"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled get: got %v, want context.Canceled", err)
	}
}
