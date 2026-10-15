package store

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/azmi2409/forge-tasks/bus"
)

// MemoryStore is a concurrency-safe in-memory Store.
//
// It is the reference implementation used by tests and local development. It
// mirrors the Postgres adapter's semantics (see migrations/001_init.sql):
// per-project atomic key counters, optimistic-concurrency updates, and
// never-reused keys.
type MemoryStore struct {
	mu               sync.RWMutex
	projects         map[string]Project
	items            map[string]WorkItem
	agents           map[string]AgentPrincipal
	counters         map[string]*atomic.Int64 // per-project key counters
	claims           map[string]Claim         // work item key -> active lease
	workflows        map[string][]StoredWorkflow
	projectWorkflows map[string]ProjectWorkflow
	evalRuns         map[string]EvalRun
	artifacts        map[string]Artifact
	artifactBlobs    map[string][]byte
	wfSeq            atomic.Int64
	evalSeq          atomic.Int64
	artifactSeq      atomic.Int64
	eventBus         *bus.Bus
}

// SetBus attaches the event bus. Mutations publish events while the store
// lock is held, so subscribers must never call back into the store.
func (s *MemoryStore) SetBus(b *bus.Bus) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.eventBus = b
}

// publish emits an event when a bus is attached.
func (s *MemoryStore) publish(typ string, data any) {
	if s.eventBus != nil {
		s.eventBus.Publish(typ, data)
	}
}

// NewMemoryStore returns an empty MemoryStore.
func NewMemoryStore() *MemoryStore {
	s := &MemoryStore{
		projects:         make(map[string]Project),
		items:            make(map[string]WorkItem),
		agents:           make(map[string]AgentPrincipal),
		counters:         make(map[string]*atomic.Int64),
		claims:           make(map[string]Claim),
		workflows:        make(map[string][]StoredWorkflow),
		projectWorkflows: make(map[string]ProjectWorkflow),
		evalRuns:         make(map[string]EvalRun),
		artifacts:        make(map[string]Artifact),
		artifactBlobs:    make(map[string][]byte),
	}
	s.seedDefaultWorkflowLocked()
	return s
}

// Compile-time contract check.
var _ Store = (*MemoryStore)(nil)

func (s *MemoryStore) CreateProject(ctx context.Context, p Project) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := p.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.projects[p.Key]; ok {
		return fmt.Errorf("%w: project %s", ErrAlreadyExists, p.Key)
	}
	s.projects[p.Key] = p
	s.counters[p.Key] = &atomic.Int64{}
	s.pinDefaultWorkflowLocked(p.Key)
	s.publish(bus.TypeProjectCreated, p)
	return nil
}

func (s *MemoryStore) ListProjects(ctx context.Context) ([]Project, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Project, 0, len(s.projects))
	for _, p := range s.projects {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

func (s *MemoryStore) GetProject(ctx context.Context, key string) (Project, error) {
	if err := ctx.Err(); err != nil {
		return Project{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.projects[key]
	if !ok {
		return Project{}, fmt.Errorf("%w: project %s", ErrNotFound, key)
	}
	return p, nil
}

func (s *MemoryStore) NextKey(ctx context.Context, projectKey string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	// Map lookup under RLock; the counter itself is atomic so concurrent
	// allocators never block each other on the mutex.
	s.mu.RLock()
	c, ok := s.counters[projectKey]
	s.mu.RUnlock()
	if !ok {
		return "", fmt.Errorf("%w: project %s", ErrNotFound, projectKey)
	}
	n := c.Add(1)
	return fmt.Sprintf("%s-%d", projectKey, n), nil
}

func (s *MemoryStore) CreateWorkItem(ctx context.Context, w WorkItem) (WorkItem, error) {
	if err := ctx.Err(); err != nil {
		return WorkItem{}, err
	}
	if err := w.Validate(); err != nil {
		return WorkItem{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.projects[w.ProjectKey]; !ok {
		return WorkItem{}, fmt.Errorf("%w: project %s", ErrNotFound, w.ProjectKey)
	}
	key := w.Key
	if key == "" {
		c := s.counters[w.ProjectKey]
		key = fmt.Sprintf("%s-%d", w.ProjectKey, c.Add(1))
	} else {
		if !strings.HasPrefix(key, w.ProjectKey+"-") {
			return WorkItem{}, &ValidationError{Field: "key", Message: "must start with project key prefix"}
		}
		if _, taken := s.items[key]; taken {
			return WorkItem{}, fmt.Errorf("%w: work item %s", ErrAlreadyExists, key)
		}
	}
	now := time.Now().UTC()
	stored := copyWorkItem(w)
	stored.Key = key
	if stored.Status == "" {
		stored.Status = DefaultStatus
	}
	if stored.IssueType == "" {
		stored.IssueType = DefaultIssueType
	}
	if stored.Priority == "" {
		stored.Priority = DefaultPriority
	}
	stored.Version = 1
	stored.CreatedAt = now
	stored.UpdatedAt = now
	s.items[key] = stored
	s.publish(bus.TypeWorkItemCreated, copyWorkItem(stored))
	return copyWorkItem(stored), nil
}

func (s *MemoryStore) GetWorkItem(ctx context.Context, key string) (WorkItem, error) {
	if err := ctx.Err(); err != nil {
		return WorkItem{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	w, ok := s.items[key]
	if !ok {
		return WorkItem{}, fmt.Errorf("%w: work item %s", ErrNotFound, key)
	}
	return copyWorkItem(w), nil
}

func (s *MemoryStore) UpdateWorkItem(ctx context.Context, w WorkItem) (WorkItem, error) {
	if err := ctx.Err(); err != nil {
		return WorkItem{}, err
	}
	if strings.TrimSpace(w.Key) == "" {
		return WorkItem{}, &ValidationError{Field: "key", Message: "required"}
	}
	if err := w.Validate(); err != nil {
		return WorkItem{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.items[w.Key]
	if !ok {
		return WorkItem{}, fmt.Errorf("%w: work item %s", ErrNotFound, w.Key)
	}
	if w.Version != cur.Version {
		return WorkItem{}, fmt.Errorf("%w: have version %d, want %d", ErrVersionConflict, w.Version, cur.Version)
	}
	stored := copyWorkItem(w)
	stored.Version = cur.Version + 1
	stored.CreatedAt = cur.CreatedAt
	stored.UpdatedAt = time.Now().UTC()
	s.items[w.Key] = stored
	s.publish(bus.TypeWorkItemUpdated, copyWorkItem(stored))
	return copyWorkItem(stored), nil
}

func (s *MemoryStore) DeleteWorkItem(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.items[key]; !ok {
		return fmt.Errorf("%w: work item %s", ErrNotFound, key)
	}
	delete(s.items, key)
	// Keys are never reallocated: the project counter keeps incrementing.
	s.publish(bus.TypeWorkItemDeleted, map[string]any{"key": key})
	return nil
}

func (s *MemoryStore) ListWorkItems(ctx context.Context, f WorkItemFilter) ([]WorkItem, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	limit := f.Limit
	if limit <= 0 {
		limit = DefaultListLimit
	}
	if limit > MaxListLimit {
		limit = MaxListLimit
	}
	offset := f.Offset
	if offset < 0 {
		offset = 0
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	// Collect keys first so the result order is deterministic.
	keys := make([]string, 0, len(s.items))
	for k := range s.items {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]WorkItem, 0, limit)
	skipped := 0
	for _, k := range keys {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		w := s.items[k]
		if !matchFilter(w, f) {
			continue
		}
		if skipped < offset {
			skipped++
			continue
		}
		if len(out) >= limit {
			break
		}
		out = append(out, copyWorkItem(w))
	}
	return out, nil
}

func matchFilter(w WorkItem, f WorkItemFilter) bool {
	if f.ProjectKey != "" && w.ProjectKey != f.ProjectKey {
		return false
	}
	if f.Status != "" && w.Status != f.Status {
		return false
	}
	if f.AssigneeID != "" && w.AssigneeID != f.AssigneeID {
		return false
	}
	if f.IssueType != "" && w.IssueType != f.IssueType {
		return false
	}
	for _, l := range f.Labels {
		found := false
		for _, wl := range w.Labels {
			if wl == l {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func (s *MemoryStore) UpsertAgent(ctx context.Context, a AgentPrincipal) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := a.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.agents[a.ID] = copyAgent(a)
	return nil
}

func (s *MemoryStore) GetAgent(ctx context.Context, id string) (AgentPrincipal, error) {
	if err := ctx.Err(); err != nil {
		return AgentPrincipal{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	a, ok := s.agents[id]
	if !ok {
		return AgentPrincipal{}, fmt.Errorf("%w: agent %s", ErrNotFound, id)
	}
	return copyAgent(a), nil
}

func (s *MemoryStore) Heartbeat(ctx context.Context, id string, at time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.agents[id]
	if !ok {
		return fmt.Errorf("%w: agent %s", ErrNotFound, id)
	}
	a.LastHeartbeat = at.UTC()
	s.agents[id] = a
	return nil
}

// copyWorkItem deep-copies a work item so callers can never alias stored state.
func copyWorkItem(w WorkItem) WorkItem {
	out := w
	if w.Acceptance != nil {
		out.Acceptance = make([]AcceptanceCriterion, len(w.Acceptance))
		for i, c := range w.Acceptance {
			nc := c
			if c.PredicateArgs != nil {
				nc.PredicateArgs = make(map[string]any, len(c.PredicateArgs))
				for k, v := range c.PredicateArgs {
					nc.PredicateArgs[k] = v
				}
			}
			if c.EvaluatedAt != nil {
				t := *c.EvaluatedAt
				nc.EvaluatedAt = &t
			}
			out.Acceptance[i] = nc
		}
	}
	if w.Labels != nil {
		out.Labels = append([]string(nil), w.Labels...)
	}
	if w.CustomFields != nil {
		out.CustomFields = make(map[string]any, len(w.CustomFields))
		for k, v := range w.CustomFields {
			out.CustomFields[k] = v
		}
	}
	return out
}

func copyAgent(a AgentPrincipal) AgentPrincipal {
	out := a
	if a.Capabilities != nil {
		out.Capabilities = append([]string(nil), a.Capabilities...)
	}
	return out
}
