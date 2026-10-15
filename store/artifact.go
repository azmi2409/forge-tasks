package store

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"
)

// Artifact kinds. Unknown kinds are rejected: strict schemas keep the
// surface agent-legible.
const (
	ArtifactPlan       = "plan"
	ArtifactTestReport = "test_report"
	ArtifactDiff       = "diff"
	ArtifactLog        = "log"
	ArtifactScreenshot = "screenshot"
	ArtifactEvalResult = "eval_result"
)

// MaxArtifactBytes caps a single artifact blob (10 MiB).
const MaxArtifactBytes = 10 * 1024 * 1024

// Artifact is a content-addressed, typed, versioned attachment: plans agents
// can diff, test reports criteria can reference, eval outputs, logs.
type Artifact struct {
	ID          string         `json:"id"` // art-<n>
	SHA256      string         `json:"sha256"`
	Kind        string         `json:"kind"`
	Name        string         `json:"name"`
	WorkItemKey string         `json:"work_item_key,omitempty"`
	Metadata    map[string]any `json:"metadata,omitempty"`
	Version     int            `json:"version"`
	Size        int64          `json:"size"`
	CreatedBy   string         `json:"created_by,omitempty"`
	CreatedAt   time.Time      `json:"created_at"`
}

// ArtifactInput is the caller-supplied upload.
type ArtifactInput struct {
	Kind        string
	Name        string
	WorkItemKey string
	Content     []byte
	Metadata    map[string]any
	CreatedBy   string
}

// ArtifactFilter scopes listings.
type ArtifactFilter struct {
	WorkItemKey string
	Kind        string
	Limit       int
	Offset      int
}

// Validate checks kind, name, size, and kind-specific metadata schemas.
func (in ArtifactInput) Validate() error {
	switch in.Kind {
	case ArtifactPlan, ArtifactTestReport, ArtifactDiff, ArtifactLog, ArtifactScreenshot, ArtifactEvalResult:
	default:
		return &ValidationError{Field: "kind", Message: fmt.Sprintf("unknown kind %q", in.Kind)}
	}
	if in.Name == "" {
		return &ValidationError{Field: "name", Message: "required"}
	}
	if len(in.Content) == 0 {
		return &ValidationError{Field: "content", Message: "empty"}
	}
	if len(in.Content) > MaxArtifactBytes {
		return &ValidationError{Field: "content",
			Message: fmt.Sprintf("exceeds %d bytes", MaxArtifactBytes)}
	}
	return validateArtifactMetadata(in.Kind, in.Metadata)
}

// validateArtifactMetadata enforces per-kind schemas on the sidecar.
func validateArtifactMetadata(kind string, meta map[string]any) error {
	require := func(key string) error {
		if meta == nil {
			return &ValidationError{Field: "metadata." + key, Message: "required"}
		}
		v, ok := meta[key]
		if !ok {
			return &ValidationError{Field: "metadata." + key, Message: "required"}
		}
		_ = v
		return nil
	}
	switch kind {
	case ArtifactTestReport:
		if err := require("status"); err != nil {
			return err
		}
		if s, _ := meta["status"].(string); s != "pass" && s != "fail" {
			return &ValidationError{Field: "metadata.status", Message: `must be "pass" or "fail"`}
		}
	case ArtifactEvalResult:
		if err := require("passed"); err != nil {
			return err
		}
		if _, ok := meta["passed"].(bool); !ok {
			return &ValidationError{Field: "metadata.passed", Message: "must be a boolean"}
		}
	}
	return nil
}

// sha256Hex content-addresses a blob.
func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
