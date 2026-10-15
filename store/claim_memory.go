package store

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/azmi2409/forge-tasks/bus"
)

// Memory claim/lease implementation. Expired leases are purged lazily inside
// each mutating call, so no background reaper is needed for correctness.

func (s *MemoryStore) Claim(ctx context.Context, agentID string, f WorkItemFilter, ttl time.Duration) (WorkItem, Claim, error) {
	if err := ctx.Err(); err != nil {
		return WorkItem{}, Claim{}, err
	}
	ttl, err := validateTTL(ttl)
	if err != nil {
		return WorkItem{}, Claim{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.agents[agentID]; !ok {
		return WorkItem{}, Claim{}, fmt.Errorf("%w: agent %s", ErrNotFound, agentID)
	}
	now := time.Now().UTC()
	s.purgeExpiredLocked(now)
	if f.Status == "" {
		f.Status = DefaultClaimableStatus
	}
	// Deterministic pick: lowest key wins, so concurrent agents converge
	// instead of thundering on the same row.
	keys := make([]string, 0, len(s.items))
	for k := range s.items {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		w := s.items[k]
		if !matchFilter(w, f) {
			continue
		}
		if _, held := s.claims[k]; held {
			continue
		}
		c := Claim{
			WorkItemKey: k,
			AgentID:     agentID,
			ClaimedAt:   now,
			ExpiresAt:   now.Add(ttl),
		}
		s.claims[k] = c
		s.publish(bus.TypeClaimCreated, c)
		return copyWorkItem(w), c, nil
	}
	return WorkItem{}, Claim{}, ErrNoWorkAvailable
}

func (s *MemoryStore) RenewClaim(ctx context.Context, key, agentID string, ttl time.Duration) (Claim, error) {
	if err := ctx.Err(); err != nil {
		return Claim{}, err
	}
	ttl, err := validateTTL(ttl)
	if err != nil {
		return Claim{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	s.purgeExpiredLocked(now)
	c, ok := s.claims[key]
	if !ok {
		return Claim{}, fmt.Errorf("%w: claim on %s", ErrNotFound, key)
	}
	if c.AgentID != agentID {
		return Claim{}, fmt.Errorf("%w: %s held by %s", ErrClaimConflict, key, c.AgentID)
	}
	c.ExpiresAt = now.Add(ttl)
	s.claims[key] = c
	s.publish(bus.TypeClaimRenewed, c)
	return c, nil
}

func (s *MemoryStore) ReleaseClaim(ctx context.Context, key, agentID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.purgeExpiredLocked(time.Now().UTC())
	c, ok := s.claims[key]
	if !ok {
		return fmt.Errorf("%w: claim on %s", ErrNotFound, key)
	}
	if c.AgentID != agentID {
		return fmt.Errorf("%w: %s held by %s", ErrClaimConflict, key, c.AgentID)
	}
	delete(s.claims, key)
	s.publish(bus.TypeClaimReleased, map[string]any{"key": key, "agent_id": agentID})
	return nil
}

func (s *MemoryStore) GetClaim(ctx context.Context, key string) (Claim, error) {
	if err := ctx.Err(); err != nil {
		return Claim{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.purgeExpiredLocked(time.Now().UTC())
	c, ok := s.claims[key]
	if !ok {
		return Claim{}, fmt.Errorf("%w: claim on %s", ErrNotFound, key)
	}
	return c, nil
}

func (s *MemoryStore) ListClaims(ctx context.Context, agentID string) ([]Claim, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	s.purgeExpiredLocked(now)
	var out []Claim
	for _, c := range s.claims {
		if c.AgentID == agentID {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ExpiresAt.Before(out[j].ExpiresAt) })
	return out, nil
}

// purgeExpiredLocked drops lapsed leases. Callers must hold s.mu.
func (s *MemoryStore) purgeExpiredLocked(now time.Time) {
	for k, c := range s.claims {
		if c.Expired(now) {
			delete(s.claims, k)
		}
	}
}
