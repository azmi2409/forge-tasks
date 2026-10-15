// Package mcp is forge-tasks' MCP server (component 3): the primary,
// designed-first interface for AI agents. It exposes typed tools with
// strict JSON schemas over the service layer — never over HTTP, never
// duplicating business logic. Transports: stdio (see cmd/forge-tasks-mcp)
// and, later, streamable HTTP.
package mcp

import (
	"context"
	"encoding/base64"
	"errors"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/azmi2409/forge-tasks/service"
	"github.com/azmi2409/forge-tasks/store"
	"github.com/azmi2409/forge-tasks/workflow"
)

// NewServer builds the MCP server over the service layer.
func NewServer(svc *service.Service) *sdk.Server {
	s := sdk.NewServer(
		&sdk.Implementation{Name: "forge-tasks", Version: "0.1.0"},
		&sdk.ServerOptions{},
	)
	h := &handlers{svc: svc}
	sdk.AddTool(s, &sdk.Tool{Name: "list_projects", Description: "List all projects."},
		h.listProjects)
	sdk.AddTool(s, &sdk.Tool{Name: "create_project", Description: "Create a project. New projects start on the default workflow (todo -> doing -> done)."},
		h.createProject)
	sdk.AddTool(s, &sdk.Tool{Name: "create_work_item", Description: "Create a work item. The key (e.g. FT-1) is allocated atomically."},
		h.createWorkItem)
	sdk.AddTool(s, &sdk.Tool{Name: "get_work_item", Description: "Get one work item by key."},
		h.getWorkItem)
	sdk.AddTool(s, &sdk.Tool{Name: "update_work_item", Description: "Sparse-update a work item (optimistic concurrency via version). Status changes are rejected here: use the transition tool so the workflow engine governs them."},
		h.updateWorkItem)
	sdk.AddTool(s, &sdk.Tool{Name: "list_work_items", Description: "List work items with optional filters and pagination."},
		h.listWorkItems)
	sdk.AddTool(s, &sdk.Tool{Name: "claim_task", Description: "Atomically claim one available work item as a lease. Returns claimed=false when the queue is empty; poll again later."},
		h.claimTask)
	sdk.AddTool(s, &sdk.Tool{Name: "renew_claim", Description: "Extend (heartbeat) a claim lease you hold."},
		h.renewClaim)
	sdk.AddTool(s, &sdk.Tool{Name: "release_claim", Description: "Release a claim lease early so others can take the work."},
		h.releaseClaim)
	sdk.AddTool(s, &sdk.Tool{Name: "list_transitions", Description: "List the legal status transitions from a work item's current state, with their preconditions."},
		h.listTransitions)
	sdk.AddTool(s, &sdk.Tool{Name: "transition", Description: "Move a work item to a new status through the workflow engine. Preconditions are checked and eval hooks run; a refusal returns allowed=false with machine-readable reasons (never an error). Use dry_run to ask \"would this pass?\" without applying."},
		h.transition)
	sdk.AddTool(s, &sdk.Tool{Name: "upsert_agent", Description: "Create or replace an agent principal (register before claiming work)."},
		h.upsertAgent)
	sdk.AddTool(s, &sdk.Tool{Name: "heartbeat", Description: "Record agent liveness."},
		h.heartbeat)
	sdk.AddTool(s, &sdk.Tool{Name: "list_eval_runs", Description: "Show a work item's eval-hook history, newest first."},
		h.listEvalRuns)
	sdk.AddTool(s, &sdk.Tool{Name: "add_artifact", Description: "Attach a typed, versioned artifact (plan, test_report, diff, log, screenshot, eval_result) to a work item. Content is content-addressed by SHA-256; re-uploading the same name creates a new version for diffing."},
		h.addArtifact)
	sdk.AddTool(s, &sdk.Tool{Name: "get_artifact", Description: "Get an artifact's metadata and content."},
		h.getArtifact)
	sdk.AddTool(s, &sdk.Tool{Name: "list_artifacts", Description: "List artifacts, optionally filtered by work item or kind, newest first."},
		h.listArtifacts)
	return s
}

type handlers struct{ svc *service.Service }

func (h *handlers) store() store.Store { return h.svc.Store }

// --- projects ---

type listProjectsOut struct {
	Projects []store.Project `json:"projects"`
}

func (h *handlers) listProjects(ctx context.Context, req *sdk.CallToolRequest, _ struct{}) (*sdk.CallToolResult, listProjectsOut, error) {
	projects, err := h.store().ListProjects(ctx)
	if err != nil {
		return nil, listProjectsOut{}, err
	}
	if projects == nil {
		projects = []store.Project{}
	}
	return nil, listProjectsOut{Projects: projects}, nil
}

type createProjectIn struct {
	Key  string `json:"key" jsonschema:"project key, e.g. FT"`
	Name string `json:"name" jsonschema:"human-readable name"`
}

func (h *handlers) createProject(ctx context.Context, req *sdk.CallToolRequest, in createProjectIn) (*sdk.CallToolResult, store.Project, error) {
	p := store.Project{Key: in.Key, Name: in.Name}
	if err := h.store().CreateProject(ctx, p); err != nil {
		return nil, store.Project{}, err
	}
	return nil, p, nil
}

// --- work items ---

type createWorkItemIn struct {
	ProjectKey  string                      `json:"project_key"`
	Title       string                      `json:"title"`
	Description string                      `json:"description,omitempty"`
	IssueType   string                      `json:"issue_type,omitempty"`
	Priority    string                      `json:"priority,omitempty"`
	AssigneeID  string                      `json:"assignee_id,omitempty"`
	Labels      []string                    `json:"labels,omitempty"`
	Acceptance  []store.AcceptanceCriterion `json:"acceptance,omitempty"`
}

func (h *handlers) createWorkItem(ctx context.Context, req *sdk.CallToolRequest, in createWorkItemIn) (*sdk.CallToolResult, store.WorkItem, error) {
	w, err := h.store().CreateWorkItem(ctx, store.WorkItem{
		ProjectKey:  in.ProjectKey,
		Title:       in.Title,
		Description: in.Description,
		IssueType:   in.IssueType,
		Priority:    in.Priority,
		AssigneeID:  in.AssigneeID,
		Labels:      in.Labels,
		Acceptance:  in.Acceptance,
	})
	if err != nil {
		return nil, store.WorkItem{}, err
	}
	return nil, w, nil
}

type getWorkItemIn struct {
	Key string `json:"key" jsonschema:"work item key, e.g. FT-1"`
}

func (h *handlers) getWorkItem(ctx context.Context, req *sdk.CallToolRequest, in getWorkItemIn) (*sdk.CallToolResult, store.WorkItem, error) {
	w, err := h.store().GetWorkItem(ctx, in.Key)
	if err != nil {
		return nil, store.WorkItem{}, err
	}
	return nil, w, nil
}

type updateWorkItemIn struct {
	Key         string                      `json:"key"`
	Version     int64                       `json:"version" jsonschema:"the version you read; stale versions are rejected"`
	Title       *string                     `json:"title,omitempty"`
	Description *string                     `json:"description,omitempty"`
	IssueType   *string                     `json:"issue_type,omitempty"`
	Priority    *string                     `json:"priority,omitempty"`
	AssigneeID  *string                     `json:"assignee_id,omitempty"`
	Labels      []string                    `json:"labels,omitempty"`
	Acceptance  []store.AcceptanceCriterion `json:"acceptance,omitempty"`
}

func (h *handlers) updateWorkItem(ctx context.Context, req *sdk.CallToolRequest, in updateWorkItemIn) (*sdk.CallToolResult, store.WorkItem, error) {
	w, err := h.store().GetWorkItem(ctx, in.Key)
	if err != nil {
		return nil, store.WorkItem{}, err
	}
	w.Version = in.Version
	if in.Title != nil {
		w.Title = *in.Title
	}
	if in.Description != nil {
		w.Description = *in.Description
	}
	if in.IssueType != nil {
		w.IssueType = *in.IssueType
	}
	if in.Priority != nil {
		w.Priority = *in.Priority
	}
	if in.AssigneeID != nil {
		w.AssigneeID = *in.AssigneeID
	}
	if in.Labels != nil {
		w.Labels = in.Labels
	}
	if in.Acceptance != nil {
		w.Acceptance = in.Acceptance
	}
	updated, err := h.store().UpdateWorkItem(ctx, w)
	if err != nil {
		return nil, store.WorkItem{}, err
	}
	return nil, updated, nil
}

type listWorkItemsIn struct {
	Project   string   `json:"project,omitempty"`
	Status    string   `json:"status,omitempty"`
	Assignee  string   `json:"assignee,omitempty"`
	IssueType string   `json:"issue_type,omitempty"`
	Labels    []string `json:"labels,omitempty"`
	Limit     int      `json:"limit,omitempty"`
	Offset    int      `json:"offset,omitempty"`
}

type listWorkItemsOut struct {
	Items []store.WorkItem `json:"items"`
}

func (h *handlers) listWorkItems(ctx context.Context, req *sdk.CallToolRequest, in listWorkItemsIn) (*sdk.CallToolResult, listWorkItemsOut, error) {
	items, err := h.store().ListWorkItems(ctx, store.WorkItemFilter{
		ProjectKey: in.Project,
		Status:     in.Status,
		AssigneeID: in.Assignee,
		IssueType:  in.IssueType,
		Labels:     in.Labels,
		Limit:      in.Limit,
		Offset:     in.Offset,
	})
	if err != nil {
		return nil, listWorkItemsOut{}, err
	}
	if items == nil {
		items = []store.WorkItem{}
	}
	return nil, listWorkItemsOut{Items: items}, nil
}

// --- claims ---

type claimTaskIn struct {
	AgentID    string   `json:"agent_id"`
	TTLSeconds *int64   `json:"ttl_seconds,omitempty" jsonschema:"lease seconds, default 300"`
	Project    string   `json:"project,omitempty"`
	Labels     []string `json:"labels,omitempty"`
}

type claimTaskOut struct {
	Claimed  bool            `json:"claimed"`
	Claim    *store.Claim    `json:"claim,omitempty"`
	WorkItem *store.WorkItem `json:"work_item,omitempty"`
}

func (h *handlers) claimTask(ctx context.Context, req *sdk.CallToolRequest, in claimTaskIn) (*sdk.CallToolResult, claimTaskOut, error) {
	ttl := store.DefaultClaimTTL
	if in.TTLSeconds != nil {
		ttl = time.Duration(*in.TTLSeconds) * time.Second
	}
	item, claim, err := h.store().Claim(ctx, in.AgentID, store.WorkItemFilter{
		ProjectKey: in.Project,
		Labels:     in.Labels,
	}, ttl)
	if err == store.ErrNoWorkAvailable {
		return nil, claimTaskOut{Claimed: false}, nil
	}
	if err != nil {
		return nil, claimTaskOut{}, err
	}
	return nil, claimTaskOut{Claimed: true, Claim: &claim, WorkItem: &item}, nil
}

type claimKeyIn struct {
	Key        string `json:"key" jsonschema:"work item key"`
	AgentID    string `json:"agent_id"`
	TTLSeconds *int64 `json:"ttl_seconds,omitempty" jsonschema:"lease seconds, default 300"`
}

func (h *handlers) renewClaim(ctx context.Context, req *sdk.CallToolRequest, in claimKeyIn) (*sdk.CallToolResult, store.Claim, error) {
	ttl := store.DefaultClaimTTL
	if in.TTLSeconds != nil {
		ttl = time.Duration(*in.TTLSeconds) * time.Second
	}
	c, err := h.store().RenewClaim(ctx, in.Key, in.AgentID, ttl)
	if err != nil {
		return nil, store.Claim{}, err
	}
	return nil, c, nil
}

type releaseClaimIn struct {
	Key     string `json:"key" jsonschema:"work item key"`
	AgentID string `json:"agent_id"`
}

type releaseClaimOut struct {
	Released bool `json:"released"`
}

func (h *handlers) releaseClaim(ctx context.Context, req *sdk.CallToolRequest, in releaseClaimIn) (*sdk.CallToolResult, releaseClaimOut, error) {
	if err := h.store().ReleaseClaim(ctx, in.Key, in.AgentID); err != nil {
		return nil, releaseClaimOut{}, err
	}
	return nil, releaseClaimOut{Released: true}, nil
}

// --- transitions ---

type listTransitionsOut struct {
	Current     string                `json:"current"`
	Transitions []workflow.Transition `json:"transitions"`
}

func (h *handlers) listTransitions(ctx context.Context, req *sdk.CallToolRequest, in getWorkItemIn) (*sdk.CallToolResult, listTransitionsOut, error) {
	current, transitions, err := h.svc.AvailableTransitions(ctx, in.Key)
	if err != nil {
		return nil, listTransitionsOut{}, err
	}
	if transitions == nil {
		transitions = []workflow.Transition{}
	}
	return nil, listTransitionsOut{Current: current, Transitions: transitions}, nil
}

type transitionIn struct {
	Key     string `json:"key" jsonschema:"work item key"`
	To      string `json:"to" jsonschema:"target status"`
	Version int64  `json:"version" jsonschema:"the version you read; stale versions are rejected"`
	DryRun  bool   `json:"dry_run,omitempty" jsonschema:"check without applying"`
}

type transitionOut struct {
	Allowed     bool                 `json:"allowed"`
	WorkItem    *store.WorkItem      `json:"work_item,omitempty"`
	Transition  *workflow.Transition `json:"transition,omitempty"`
	EvalRun     *store.EvalRun       `json:"eval_run,omitempty"`
	BlockedCode string               `json:"blocked_code,omitempty"`
	Message     string               `json:"message,omitempty"`
	Missing     []string             `json:"missing,omitempty"`
	Available   []string             `json:"available,omitempty"`
}

func (h *handlers) transition(ctx context.Context, req *sdk.CallToolRequest, in transitionIn) (*sdk.CallToolResult, transitionOut, error) {
	item, err := h.store().GetWorkItem(ctx, in.Key)
	if err != nil {
		return nil, transitionOut{}, err
	}
	item.Version = in.Version
	outcome, err := h.svc.Transition(ctx, item, in.To, in.DryRun)
	if err != nil {
		var blocked *service.BlockedError
		if errors.As(err, &blocked) {
			return nil, transitionOut{
				Allowed:     false,
				BlockedCode: blocked.Result.BlockedCode,
				Message:     blocked.Result.Message,
				Missing:     blocked.Result.Missing,
				Available:   blocked.Result.Available,
				EvalRun:     blocked.EvalRun,
			}, nil
		}
		return nil, transitionOut{}, err
	}
	out := transitionOut{
		Allowed:    true,
		Transition: outcome.Transition,
		EvalRun:    outcome.EvalRun,
	}
	if !outcome.DryRun {
		out.WorkItem = &outcome.WorkItem
	}
	return nil, out, nil
}

// --- agents ---

type upsertAgentIn struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Kind         string   `json:"kind,omitempty" jsonschema:"agent or human, default agent"`
	Capabilities []string `json:"capabilities,omitempty"`
}

func (h *handlers) upsertAgent(ctx context.Context, req *sdk.CallToolRequest, in upsertAgentIn) (*sdk.CallToolResult, store.AgentPrincipal, error) {
	kind := in.Kind
	if kind == "" {
		kind = "agent"
	}
	a := store.AgentPrincipal{ID: in.ID, Name: in.Name, Kind: store.AgentKind(kind), Capabilities: in.Capabilities}
	if err := h.store().UpsertAgent(ctx, a); err != nil {
		return nil, store.AgentPrincipal{}, err
	}
	return nil, a, nil
}

type heartbeatIn struct {
	AgentID string `json:"agent_id"`
}

type heartbeatOut struct {
	AgentID string    `json:"agent_id"`
	At      time.Time `json:"at"`
}

func (h *handlers) heartbeat(ctx context.Context, req *sdk.CallToolRequest, in heartbeatIn) (*sdk.CallToolResult, heartbeatOut, error) {
	at := time.Now().UTC()
	if err := h.store().Heartbeat(ctx, in.AgentID, at); err != nil {
		return nil, heartbeatOut{}, err
	}
	return nil, heartbeatOut{AgentID: in.AgentID, At: at}, nil
}

type listEvalRunsOut struct {
	EvalRuns []store.EvalRun `json:"eval_runs"`
}

func (h *handlers) listEvalRuns(ctx context.Context, req *sdk.CallToolRequest, in getWorkItemIn) (*sdk.CallToolResult, listEvalRunsOut, error) {
	runs, err := h.store().ListEvalRuns(ctx, in.Key)
	if err != nil {
		return nil, listEvalRunsOut{}, err
	}
	if runs == nil {
		runs = []store.EvalRun{}
	}
	return nil, listEvalRunsOut{EvalRuns: runs}, nil
}

// --- artifacts ---

type addArtifactIn struct {
	Kind        string         `json:"kind" jsonschema:"plan, test_report, diff, log, screenshot, or eval_result"`
	Name        string         `json:"name" jsonschema:"human name; re-uploading the same name versions it"`
	WorkItemKey string         `json:"work_item_key,omitempty"`
	ContentB64  string         `json:"content_base64" jsonschema:"base64-encoded content, max 10 MiB"`
	Metadata    map[string]any `json:"metadata,omitempty" jsonschema:"kind-specific sidecar, e.g. {status: pass} for test_report"`
	CreatedBy   string         `json:"created_by,omitempty"`
}

func (h *handlers) addArtifact(ctx context.Context, req *sdk.CallToolRequest, in addArtifactIn) (*sdk.CallToolResult, store.Artifact, error) {
	content, err := base64.StdEncoding.DecodeString(in.ContentB64)
	if err != nil {
		return nil, store.Artifact{}, errors.New("content_base64: invalid base64")
	}
	a, err := h.store().CreateArtifact(ctx, store.ArtifactInput{
		Kind:        in.Kind,
		Name:        in.Name,
		WorkItemKey: in.WorkItemKey,
		Content:     content,
		Metadata:    in.Metadata,
		CreatedBy:   in.CreatedBy,
	})
	if err != nil {
		return nil, store.Artifact{}, err
	}
	return nil, a, nil
}

type getArtifactIn struct {
	ID string `json:"id" jsonschema:"artifact id, e.g. art-1"`
}

type getArtifactOut struct {
	store.Artifact
	ContentB64 string `json:"content_base64"`
}

func (h *handlers) getArtifact(ctx context.Context, req *sdk.CallToolRequest, in getArtifactIn) (*sdk.CallToolResult, getArtifactOut, error) {
	a, err := h.store().GetArtifact(ctx, in.ID)
	if err != nil {
		return nil, getArtifactOut{}, err
	}
	content, err := h.store().GetArtifactContent(ctx, in.ID)
	if err != nil {
		return nil, getArtifactOut{}, err
	}
	return nil, getArtifactOut{
		Artifact:   a,
		ContentB64: base64.StdEncoding.EncodeToString(content),
	}, nil
}

type listArtifactsIn struct {
	WorkItemKey string `json:"work_item_key,omitempty"`
	Kind        string `json:"kind,omitempty"`
	Limit       int    `json:"limit,omitempty"`
}

type listArtifactsOut struct {
	Artifacts []store.Artifact `json:"artifacts"`
}

func (h *handlers) listArtifacts(ctx context.Context, req *sdk.CallToolRequest, in listArtifactsIn) (*sdk.CallToolResult, listArtifactsOut, error) {
	artifacts, err := h.store().ListArtifacts(ctx, store.ArtifactFilter{
		WorkItemKey: in.WorkItemKey,
		Kind:        in.Kind,
		Limit:       in.Limit,
	})
	if err != nil {
		return nil, listArtifactsOut{}, err
	}
	if artifacts == nil {
		artifacts = []store.Artifact{}
	}
	return nil, listArtifactsOut{Artifacts: artifacts}, nil
}
