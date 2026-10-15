package store

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"sync"
	"testing"
)

// TestPostgresStore exercises the pgx adapter against a real Postgres.
// It runs in CI (postgres service) and locally with:
//
//	TEST_DATABASE_URL=postgres://forge:forge@localhost:5432/forge_tasks?sslmode=disable go test ./store/ -run TestPostgres
//
// Without TEST_DATABASE_URL the test skips.
func TestPostgresStore(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping Postgres adapter test")
	}
	ctx := context.Background()
	dbName := fmt.Sprintf("forge_test_%d", os.Getpid())

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
	defer func() {
		admin, _ := NewPostgresStore(ctx, url)
		if admin != nil {
			admin.pool.Exec(ctx, `DROP DATABASE IF EXISTS `+dbName)
			admin.Close()
		}
	}()

	testURL := dbURLWithDB(t, url, dbName)
	if err := ApplyMigration(ctx, testURL); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	s, err := NewPostgresStore(ctx, testURL)
	if err != nil {
		t.Fatalf("connect migrated: %v", err)
	}
	defer s.Close()

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

func dbURLWithDB(t *testing.T, rawurl, db string) string {
	t.Helper()
	u, err := url.Parse(rawurl)
	if err != nil {
		t.Fatalf("bad TEST_DATABASE_URL: %v", err)
	}
	u.Path = "/" + db
	return u.String()
}
