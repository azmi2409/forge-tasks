package store

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func testClaimStore(t *testing.T) *MemoryStore {
	t.Helper()
	s := NewMemoryStore()
	ctx := context.Background()
	if err := s.CreateProject(ctx, Project{Key: "FT", Name: "Forge"}); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	for _, id := range []string{"agent-1", "agent-2"} {
		if err := s.UpsertAgent(ctx, AgentPrincipal{ID: id, Name: id, Kind: AgentKindAgent}); err != nil {
			t.Fatalf("seed agent: %v", err)
		}
	}
	return s
}

func TestClaimRoundtrip(t *testing.T) {
	s := testClaimStore(t)
	ctx := context.Background()
	if _, err := s.CreateWorkItem(ctx, WorkItem{ProjectKey: "FT", Title: "task"}); err != nil {
		t.Fatalf("create: %v", err)
	}

	w, c, err := s.Claim(ctx, "agent-1", WorkItemFilter{}, time.Minute)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if w.Key != "FT-1" || c.WorkItemKey != "FT-1" || c.AgentID != "agent-1" {
		t.Fatalf("claim=%+v item=%+v", c, w)
	}
	if c.ExpiresAt.Before(c.ClaimedAt.Add(time.Minute - time.Second)) {
		t.Fatalf("ttl not applied: %+v", c)
	}

	// Second agent gets nothing: the item is leased.
	if _, _, err := s.Claim(ctx, "agent-2", WorkItemFilter{}, time.Minute); !errors.Is(err, ErrNoWorkAvailable) {
		t.Fatalf("double claim: got %v, want ErrNoWorkAvailable", err)
	}

	got, err := s.GetClaim(ctx, "FT-1")
	if err != nil || got.AgentID != "agent-1" {
		t.Fatalf("get claim: %v %+v", err, got)
	}
	list, err := s.ListClaims(ctx, "agent-1")
	if err != nil || len(list) != 1 {
		t.Fatalf("list claims: %v %+v", err, list)
	}

	// Renew extends the lease.
	renewed, err := s.RenewClaim(ctx, "FT-1", "agent-1", 10*time.Minute)
	if err != nil {
		t.Fatalf("renew: %v", err)
	}
	if renewed.ExpiresAt.Before(c.ExpiresAt) {
		t.Fatalf("renew did not extend: %+v", renewed)
	}
	// Wrong agent cannot renew or release.
	if _, err := s.RenewClaim(ctx, "FT-1", "agent-2", time.Minute); !errors.Is(err, ErrClaimConflict) {
		t.Fatalf("renew wrong agent: got %v, want ErrClaimConflict", err)
	}
	if err := s.ReleaseClaim(ctx, "FT-1", "agent-2"); !errors.Is(err, ErrClaimConflict) {
		t.Fatalf("release wrong agent: got %v, want ErrClaimConflict", err)
	}

	// Release frees the item for others.
	if err := s.ReleaseClaim(ctx, "FT-1", "agent-1"); err != nil {
		t.Fatalf("release: %v", err)
	}
	if _, _, err := s.Claim(ctx, "agent-2", WorkItemFilter{}, time.Minute); err != nil {
		t.Fatalf("re-claim after release: %v", err)
	}
}

func TestClaimExpiry(t *testing.T) {
	s := testClaimStore(t)
	ctx := context.Background()
	if _, err := s.CreateWorkItem(ctx, WorkItem{ProjectKey: "FT", Title: "task"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, _, err := s.Claim(ctx, "agent-1", WorkItemFilter{}, 50*time.Millisecond); err != nil {
		t.Fatalf("claim: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	// Lease lapsed: another agent can claim, and the old claim reads as gone.
	if _, _, err := s.Claim(ctx, "agent-2", WorkItemFilter{}, time.Minute); err != nil {
		t.Fatalf("claim after expiry: %v", err)
	}
	if _, err := s.RenewClaim(ctx, "FT-1", "agent-1", time.Minute); !errors.Is(err, ErrClaimConflict) {
		t.Fatalf("renew lapsed lease: got %v, want ErrClaimConflict", err)
	}
}

func TestClaimValidation(t *testing.T) {
	s := testClaimStore(t)
	ctx := context.Background()
	if _, err := s.CreateWorkItem(ctx, WorkItem{ProjectKey: "FT", Title: "task"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, _, err := s.Claim(ctx, "ghost", WorkItemFilter{}, time.Minute); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown agent: got %v, want ErrNotFound", err)
	}
	if _, _, err := s.Claim(ctx, "agent-1", WorkItemFilter{}, 0); err == nil {
		t.Fatal("zero ttl: got nil, want validation error")
	}
	if _, _, err := s.Claim(ctx, "agent-1", WorkItemFilter{}, 48*time.Hour); err == nil {
		t.Fatal("huge ttl: got nil, want validation error")
	}
	// Status filter respected; default is todo.
	if _, _, err := s.Claim(ctx, "agent-1", WorkItemFilter{Status: "doing"}, time.Minute); !errors.Is(err, ErrNoWorkAvailable) {
		t.Fatalf("status filter: got %v, want ErrNoWorkAvailable", err)
	}
}

// TestConcurrentClaimRace: many agents race for one item; exactly one wins.
func TestConcurrentClaimRace(t *testing.T) {
	s := testClaimStore(t)
	ctx := context.Background()
	if _, err := s.CreateWorkItem(ctx, WorkItem{ProjectKey: "FT", Title: "race"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	const n = 32
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins := map[string]int{}
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
				wins[agent]++
			} else if !errors.Is(err, ErrNoWorkAvailable) {
				t.Errorf("unexpected: %v", err)
			}
		}(i)
	}
	wg.Wait()
	total := 0
	for _, w := range wins {
		total += w
	}
	if total != 1 {
		t.Fatalf("total wins=%d, want 1 (wins=%v)", total, wins)
	}
}
