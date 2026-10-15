package store

import (
	"errors"
	"time"
)

var (
	// ErrNoWorkAvailable means the claim filter matched nothing claimable
	// right now. Callers should poll again later.
	ErrNoWorkAvailable = errors.New("store: no work available")
	// ErrClaimConflict means the work item is claimed by a different agent.
	ErrClaimConflict = errors.New("store: claim conflict")
)

// Lease bounds. Agents hold work for a bounded time; a dead agent's work
// must return to the pool quickly, but heartbeats shouldn't hammer the DB.
const (
	// DefaultClaimTTL is used when the caller doesn't specify a TTL.
	DefaultClaimTTL = 5 * time.Minute
	// MaxClaimTTL caps a single lease; renew to hold longer.
	MaxClaimTTL = 24 * time.Hour
	// DefaultClaimableStatus is the status claimed when the filter names none.
	// The workflow engine (component 5) will make this configurable per project.
	DefaultClaimableStatus = "todo"
)

// Claim is a lease of a work item by an agent principal.
type Claim struct {
	WorkItemKey string    `json:"work_item_key"`
	AgentID     string    `json:"agent_id"`
	ClaimedAt   time.Time `json:"claimed_at"`
	ExpiresAt   time.Time `json:"expires_at"`
}

// Expired reports whether the lease has lapsed as of now.
func (c Claim) Expired(now time.Time) bool { return !now.Before(c.ExpiresAt) }

// validateTTL normalizes and bounds a lease TTL.
func validateTTL(ttl time.Duration) (time.Duration, error) {
	if ttl <= 0 {
		return 0, &ValidationError{Field: "ttl", Message: "must be positive"}
	}
	if ttl > MaxClaimTTL {
		return 0, &ValidationError{Field: "ttl", Message: "exceeds maximum lease"}
	}
	return ttl, nil
}
