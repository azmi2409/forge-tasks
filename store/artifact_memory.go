package store

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/azmi2409/forge-tasks/bus"
)

func (s *MemoryStore) CreateArtifact(ctx context.Context, in ArtifactInput) (Artifact, error) {
	if err := ctx.Err(); err != nil {
		return Artifact{}, err
	}
	if err := in.Validate(); err != nil {
		return Artifact{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if in.WorkItemKey != "" {
		if _, ok := s.items[in.WorkItemKey]; !ok {
			return Artifact{}, fmt.Errorf("%w: work item %s", ErrNotFound, in.WorkItemKey)
		}
	}
	// Version: max existing for (work_item, name) + 1.
	version := 1
	for _, a := range s.artifacts {
		if a.WorkItemKey == in.WorkItemKey && a.Name == in.Name && a.Version >= version {
			version = a.Version + 1
		}
	}
	sum := sha256Hex(in.Content)
	if _, ok := s.artifactBlobs[sum]; !ok {
		cp := make([]byte, len(in.Content))
		copy(cp, in.Content)
		s.artifactBlobs[sum] = cp
	}
	meta := in.Metadata
	if meta == nil {
		meta = map[string]any{}
	}
	// Normalize through JSON so Postgres JSONB and memory agree on types.
	meta = normalizeJSONMap(meta)
	a := Artifact{
		ID:          fmt.Sprintf("art-%d", s.artifactSeq.Add(1)),
		SHA256:      sum,
		Kind:        in.Kind,
		Name:        in.Name,
		WorkItemKey: in.WorkItemKey,
		Metadata:    meta,
		Version:     version,
		Size:        int64(len(in.Content)),
		CreatedBy:   in.CreatedBy,
		CreatedAt:   time.Now().UTC(),
	}
	s.artifacts[a.ID] = a
	s.publish(bus.TypeArtifactCreated, a)
	return a, nil
}

func (s *MemoryStore) GetArtifact(ctx context.Context, id string) (Artifact, error) {
	if err := ctx.Err(); err != nil {
		return Artifact{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	a, ok := s.artifacts[id]
	if !ok {
		return Artifact{}, fmt.Errorf("%w: artifact %s", ErrNotFound, id)
	}
	return a, nil
}

func (s *MemoryStore) GetArtifactContent(ctx context.Context, id string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	a, ok := s.artifacts[id]
	if !ok {
		return nil, fmt.Errorf("%w: artifact %s", ErrNotFound, id)
	}
	blob, ok := s.artifactBlobs[a.SHA256]
	if !ok {
		return nil, fmt.Errorf("store: blob missing for artifact %s", id)
	}
	cp := make([]byte, len(blob))
	copy(cp, blob)
	return cp, nil
}

func (s *MemoryStore) ListArtifacts(ctx context.Context, f ArtifactFilter) ([]Artifact, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []Artifact
	for _, a := range s.artifacts {
		if f.WorkItemKey != "" && a.WorkItemKey != f.WorkItemKey {
			continue
		}
		if f.Kind != "" && a.Kind != f.Kind {
			continue
		}
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	limit, offset := f.Limit, f.Offset
	if offset > len(out) {
		return []Artifact{}, nil
	}
	out = out[offset:]
	if limit > 0 && limit < len(out) {
		out = out[:limit]
	}
	return out, nil
}

// normalizeJSONMap round-trips through JSON so numbers decode as float64,
// matching what Postgres JSONB returns.
func normalizeJSONMap(m map[string]any) map[string]any {
	raw, err := json.Marshal(m)
	if err != nil {
		return m
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return m
	}
	return out
}
