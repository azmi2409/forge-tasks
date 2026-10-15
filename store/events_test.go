package store

import (
	"context"
	"testing"
	"time"

	"github.com/azmi2409/forge-tasks/bus"
)

func TestMemoryStorePublishesEvents(t *testing.T) {
	b := bus.New()
	s := NewMemoryStore()
	s.SetBus(b)
	ctx := context.Background()

	// Subscribe before mutating (fromSeq=0: live-only).
	subCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := b.Subscribe(subCtx, 0)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	if err := s.CreateProject(ctx, Project{Key: "FT", Name: "Forge"}); err != nil {
		t.Fatalf("project: %v", err)
	}
	w, err := s.CreateWorkItem(ctx, WorkItem{ProjectKey: "FT", Title: "task"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.UpsertAgent(ctx, AgentPrincipal{ID: "a1", Name: "a1", Kind: AgentKindAgent}); err != nil {
		t.Fatalf("agent: %v", err)
	}
	if _, _, err := s.Claim(ctx, "a1", WorkItemFilter{}, time.Minute); err != nil {
		t.Fatalf("claim: %v", err)
	}
	w.Status = "doing"
	w.Version = 1
	if _, err := s.UpdateWorkItem(ctx, w); err != nil {
		t.Fatalf("update: %v", err)
	}

	want := []string{
		bus.TypeProjectCreated,
		bus.TypeWorkItemCreated,
		bus.TypeClaimCreated,
		bus.TypeWorkItemUpdated,
	}
	timeout := time.After(3 * time.Second)
	for i, typ := range want {
		select {
		case ev := <-ch:
			if ev.Type != typ {
				t.Fatalf("event %d: got %s, want %s", i, ev.Type, typ)
			}
			if ev.Seq != uint64(i+1) {
				t.Fatalf("event %d: seq=%d", i, ev.Seq)
			}
		case <-timeout:
			t.Fatalf("timeout waiting for event %d (%s)", i, typ)
		}
	}
}
