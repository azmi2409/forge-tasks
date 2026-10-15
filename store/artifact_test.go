package store

import (
	"context"
	"errors"
	"testing"
)

func TestArtifactValidation(t *testing.T) {
	cases := map[string]ArtifactInput{
		"unknown kind":   {Kind: "bogus", Name: "x", Content: []byte("x")},
		"empty name":     {Kind: ArtifactPlan, Content: []byte("x")},
		"empty content":  {Kind: ArtifactPlan, Name: "x"},
		"oversize":       {Kind: ArtifactPlan, Name: "x", Content: make([]byte, MaxArtifactBytes+1)},
		"report no meta": {Kind: ArtifactTestReport, Name: "x", Content: []byte("x")},
		"report bad":     {Kind: ArtifactTestReport, Name: "x", Content: []byte("x"), Metadata: map[string]any{"status": "maybe"}},
		"eval no meta":   {Kind: ArtifactEvalResult, Name: "x", Content: []byte("x")},
	}
	for name, in := range cases {
		if err := in.Validate(); err == nil {
			t.Errorf("%s: got nil, want error", name)
		}
	}
	ok := []ArtifactInput{
		{Kind: ArtifactPlan, Name: "p", Content: []byte("x")},
		{Kind: ArtifactTestReport, Name: "r", Content: []byte("x"), Metadata: map[string]any{"status": "pass"}},
		{Kind: ArtifactEvalResult, Name: "e", Content: []byte("x"), Metadata: map[string]any{"passed": true}},
	}
	for i, in := range ok {
		if err := in.Validate(); err != nil {
			t.Errorf("ok %d: %v", i, err)
		}
	}
}

func TestArtifactCRUD(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	if err := s.CreateProject(ctx, Project{Key: "FT", Name: "Forge"}); err != nil {
		t.Fatalf("project: %v", err)
	}
	w, err := s.CreateWorkItem(ctx, WorkItem{ProjectKey: "FT", Title: "task"})
	if err != nil {
		t.Fatalf("item: %v", err)
	}

	// Unknown work item rejected.
	_, err = s.CreateArtifact(ctx, ArtifactInput{
		Kind: ArtifactPlan, Name: "p", WorkItemKey: "FT-99", Content: []byte("x"),
	})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("bad item: got %v", err)
	}

	a1, err := s.CreateArtifact(ctx, ArtifactInput{
		Kind: ArtifactPlan, Name: "plan.md", WorkItemKey: w.Key,
		Content: []byte("# v1"), CreatedBy: "a1",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if a1.Version != 1 || a1.SHA256 == "" || a1.Size != 4 {
		t.Fatalf("a1=%+v", a1)
	}
	// Same name re-uploaded -> new version, same content hash only if same bytes.
	a2, err := s.CreateArtifact(ctx, ArtifactInput{
		Kind: ArtifactPlan, Name: "plan.md", WorkItemKey: w.Key,
		Content: []byte("# v2"), CreatedBy: "a1",
	})
	if err != nil {
		t.Fatalf("create2: %v", err)
	}
	if a2.Version != 2 || a2.ID == a1.ID {
		t.Fatalf("a2=%+v", a2)
	}
	// Identical content dedupes the blob (different version, same hash).
	a3, err := s.CreateArtifact(ctx, ArtifactInput{
		Kind: ArtifactPlan, Name: "plan.md", WorkItemKey: w.Key,
		Content: []byte("# v2"), CreatedBy: "a1",
	})
	if err != nil {
		t.Fatalf("create3: %v", err)
	}
	if a3.SHA256 != a2.SHA256 || a3.Version != 3 {
		t.Fatalf("a3=%+v", a3)
	}
	if len(s.artifactBlobs) != 2 {
		t.Fatalf("blobs=%d, want 2 (dedupe)", len(s.artifactBlobs))
	}

	got, err := s.GetArtifact(ctx, a1.ID)
	if err != nil || got.SHA256 != a1.SHA256 {
		t.Fatalf("get=%+v err=%v", got, err)
	}
	content, err := s.GetArtifactContent(ctx, a1.ID)
	if err != nil || string(content) != "# v1" {
		t.Fatalf("content=%q err=%v", content, err)
	}
	if _, err := s.GetArtifact(ctx, "art-99"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: got %v", err)
	}

	listed, err := s.ListArtifacts(ctx, ArtifactFilter{WorkItemKey: w.Key})
	if err != nil || len(listed) != 3 {
		t.Fatalf("listed=%+v err=%v", listed, err)
	}
	// Newest first.
	if listed[0].ID != a3.ID {
		t.Fatalf("order=%+v", listed)
	}
	byKind, err := s.ListArtifacts(ctx, ArtifactFilter{Kind: ArtifactTestReport})
	if err != nil || len(byKind) != 0 {
		t.Fatalf("byKind=%+v err=%v", byKind, err)
	}
	paged, err := s.ListArtifacts(ctx, ArtifactFilter{WorkItemKey: w.Key, Limit: 2})
	if err != nil || len(paged) != 2 {
		t.Fatalf("paged=%+v err=%v", paged, err)
	}
}
