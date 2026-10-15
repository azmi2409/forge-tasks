// Package ui is forge-tasks' minimal human web UI (component 8).
// Server-rendered HTML over the service layer — no JS build, no framework.
// The API and MCP server remain the primary interfaces; this is for humans
// who want to look at the work without curl.
package ui

import (
	"embed"
	"errors"
	"html/template"
	"net/http"
	"strconv"

	"github.com/azmi2409/forge-tasks/service"
	"github.com/azmi2409/forge-tasks/store"
	"github.com/azmi2409/forge-tasks/workflow"
)

//go:embed templates/*.html
var templatesFS embed.FS

// Handler serves the human UI. Mount it at the root; the JSON API lives
// under /api/v1 and never collides.
type Handler struct {
	svc  *service.Service
	tmpl *template.Template
}

// New builds the UI handler over the service layer.
func New(svc *service.Service) *Handler {
	tmpl := template.Must(template.New("").Funcs(template.FuncMap{
		"statusClass": statusClass,
	}).ParseFS(templatesFS, "templates/*.html"))
	return &Handler{svc: svc, tmpl: tmpl}
}

func (h *Handler) store() store.Store { return h.svc.Store }

// Routes registers the UI routes on mux.
func (h *Handler) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /", h.dashboard)
	mux.HandleFunc("GET /items/{key}", h.itemDetail)
	mux.HandleFunc("POST /items/{key}/transition", h.doTransition)
}

func statusClass(s string) string {
	switch s {
	case "todo":
		return "st-todo"
	case "doing":
		return "st-doing"
	case "done":
		return "st-done"
	default:
		return "st-other"
	}
}

type dashboardData struct {
	Projects     []store.Project
	Items        []store.WorkItem
	Project      string
	Status       string
	Statuses     []string
	TotalClaims  int
	RecentEvents []recentEvent
}

type recentEvent struct {
	Type string
	Key  string
}

func (h *Handler) dashboard(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	data := dashboardData{
		Project:  q.Get("project"),
		Status:   q.Get("status"),
		Statuses: []string{"", "todo", "doing", "done"},
	}
	projects, err := h.store().ListProjects(r.Context())
	if err != nil {
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	data.Projects = projects
	items, err := h.store().ListWorkItems(r.Context(), store.WorkItemFilter{
		ProjectKey: data.Project,
		Status:     data.Status,
		Limit:      100,
	})
	if err != nil {
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	data.Items = items
	h.render(w, "dashboard.html", data)
}

type detailData struct {
	Item        store.WorkItem
	Transitions []workflow.Transition
	Claim       *store.Claim
	EvalRuns    []store.EvalRun
	Artifacts   []store.Artifact
	Blocked     *service.BlockedError
	Version     int64
}

func (h *Handler) itemDetail(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	item, err := h.store().GetWorkItem(r.Context(), key)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	data := detailData{Item: item, Version: item.Version}
	_, data.Transitions, _ = h.svc.AvailableTransitions(r.Context(), key)
	if c, err := h.store().GetClaim(r.Context(), key); err == nil {
		data.Claim = &c
	}
	data.EvalRuns, _ = h.store().ListEvalRuns(r.Context(), key)
	data.Artifacts, _ = h.store().ListArtifacts(r.Context(), store.ArtifactFilter{WorkItemKey: key})
	h.render(w, "detail.html", data)
}

func (h *Handler) doTransition(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	to := r.FormValue("to")
	version, _ := strconv.ParseInt(r.FormValue("version"), 10, 64)
	item, err := h.store().GetWorkItem(r.Context(), key)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	item.Version = version
	outcome, err := h.svc.Transition(r.Context(), item, to, false)
	if err != nil {
		var blocked *service.BlockedError
		if !errors.As(err, &blocked) {
			// Non-blocked errors (stale version etc.): show them too.
			blocked = &service.BlockedError{Result: workflow.Result{
				BlockedCode: "error",
				Message:     err.Error(),
			}}
		}
		_ = outcome
		data := detailData{Item: item, Version: item.Version, Blocked: blocked}
		_, data.Transitions, _ = h.svc.AvailableTransitions(r.Context(), key)
		if c, cerr := h.store().GetClaim(r.Context(), key); cerr == nil {
			data.Claim = &c
		}
		data.EvalRuns, _ = h.store().ListEvalRuns(r.Context(), key)
		data.Artifacts, _ = h.store().ListArtifacts(r.Context(), store.ArtifactFilter{WorkItemKey: key})
		w.WriteHeader(http.StatusUnprocessableEntity)
		h.render(w, "detail.html", data)
		return
	}
	http.Redirect(w, r, "/items/"+key, http.StatusSeeOther)
}

func (h *Handler) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.tmpl.ExecuteTemplate(w, name, data); err != nil {
		http.Error(w, "template error", http.StatusInternalServerError)
	}
}
