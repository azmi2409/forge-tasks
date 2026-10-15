package ui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/azmi2409/forge-tasks/service"
	"github.com/azmi2409/forge-tasks/store"
)

func testHandler(t *testing.T) (*Handler, *store.MemoryStore) {
	t.Helper()
	st := store.NewMemoryStore()
	return New(service.New(st, nil)), st
}

func serve(t *testing.T, h *Handler, method, path string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if form != nil {
		req = httptest.NewRequest(method, path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	rec := httptest.NewRecorder()
	mux := http.NewServeMux()
	h.Routes(mux)
	mux.ServeHTTP(rec, req)
	return rec
}

func TestDashboard(t *testing.T) {
	h, st := testHandler(t)
	ctx := context.Background()
	_ = ctx
	if err := st.CreateProject(ctx, store.Project{Key: "FT", Name: "Forge"}); err != nil {
		t.Fatalf("project: %v", err)
	}
	if _, err := st.CreateWorkItem(ctx, store.WorkItem{ProjectKey: "FT", Title: "Hello"}); err != nil {
		t.Fatalf("item: %v", err)
	}
	rec := serve(t, h, "GET", "/", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "FT-1") || !strings.Contains(body, "Hello") {
		t.Fatalf("body missing item: %.200s", body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Fatalf("content-type=%s", ct)
	}
}

func TestItemDetailAndTransition(t *testing.T) {
	h, st := testHandler(t)
	ctx := context.Background()
	if err := st.CreateProject(ctx, store.Project{Key: "FT", Name: "Forge"}); err != nil {
		t.Fatalf("project: %v", err)
	}
	if _, err := st.CreateWorkItem(ctx, store.WorkItem{ProjectKey: "FT", Title: "Task"}); err != nil {
		t.Fatalf("item: %v", err)
	}

	rec := serve(t, h, "GET", "/items/FT-1", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("detail: %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "&rarr; doing") {
		t.Fatalf("no transition button: %.300s", rec.Body.String())
	}

	// Drive the transition through the form.
	rec = serve(t, h, "POST", "/items/FT-1/transition", url.Values{
		"to": {"doing"}, "version": {"1"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("transition: %d %s", rec.Code, rec.Body.String())
	}
	rec = serve(t, h, "GET", "/items/FT-1", nil)
	if !strings.Contains(rec.Body.String(), "doing") {
		t.Fatalf("not transitioned: %.300s", rec.Body.String())
	}

	// Unknown transition renders the reasons inline (422, not a redirect).
	rec = serve(t, h, "POST", "/items/FT-1/transition", url.Values{
		"to": {"bogus"}, "version": {"2"},
	})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("blocked: %d, want 422", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "unknown_transition") {
		t.Fatalf("no reason shown: %.300s", rec.Body.String())
	}

	// Missing item -> 404.
	rec = serve(t, h, "GET", "/items/FT-99", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing: %d, want 404", rec.Code)
	}
}
