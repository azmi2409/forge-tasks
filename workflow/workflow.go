// Package workflow is forge-tasks' deterministic workflow engine
// (component 5): versioned statechart definitions, pure-function transition
// evaluation with declared preconditions, and eval hooks that gate
// terminal transitions. It is a pure library: no I/O, no store, no HTTP.
// The api package drives it; the store package persists definitions.
package workflow

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Workflow is a versioned statechart. Definitions are immutable once stored;
// updating a workflow creates a new version, and projects pin a version, so
// in-flight work items never have the rules change under them.
type Workflow struct {
	States      []State      `json:"states"`
	Transitions []Transition `json:"transitions"`
}

// State is a named status in the machine.
type State struct {
	Name     string `json:"name"`
	Initial  bool   `json:"initial,omitempty"`
	Terminal bool   `json:"terminal,omitempty"`
}

// Transition is one legal move. Preconditions are declared data, evaluated
// by pure functions, so an agent can ask "why can't I transition?" and get
// a machine-readable answer instead of a human's shrug.
type Transition struct {
	From string `json:"from"`
	To   string `json:"to"`

	// RequireCriteriaMet blocks the transition until every acceptance
	// criterion on the work item is satisfied. This is what makes "Done"
	// a checkable predicate instead of a vibe.
	RequireCriteriaMet bool `json:"require_criteria_met,omitempty"`

	// Eval runs a declared check (command/test) before the transition is
	// applied. Failure blocks the transition with structured reasons, and
	// the run is recorded for audit.
	Eval *EvalHook `json:"eval,omitempty"`
}

// EvalHook declares a check that gates a transition.
type EvalHook struct {
	Name string `json:"name"`
	// Command runs via "sh -c" with the work item context in env vars
	// (FORGE_WORK_ITEM_KEY, FORGE_TRANSITION). Exit 0 passes.
	Command string `json:"command"`
	// TimeoutSeconds bounds the run; default DefaultEvalTimeout.
	TimeoutSeconds int `json:"timeout_seconds,omitempty"`
}

// Parse validates a raw definition and returns the workflow.
func Parse(def json.RawMessage) (Workflow, error) {
	var wf Workflow
	dec := json.NewDecoder(strings.NewReader(string(def)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&wf); err != nil {
		return Workflow{}, fmt.Errorf("workflow: invalid definition: %w", err)
	}
	if err := wf.Validate(); err != nil {
		return Workflow{}, err
	}
	return wf, nil
}

// Validate checks structural invariants.
func (wf Workflow) Validate() error {
	if len(wf.States) == 0 {
		return fmt.Errorf("workflow: at least one state required")
	}
	seen := map[string]bool{}
	initials := 0
	terminals := map[string]bool{}
	for _, s := range wf.States {
		name := strings.TrimSpace(s.Name)
		if name == "" {
			return fmt.Errorf("workflow: state with empty name")
		}
		if seen[name] {
			return fmt.Errorf("workflow: duplicate state %q", name)
		}
		seen[name] = true
		if s.Initial {
			initials++
		}
		if s.Terminal {
			terminals[name] = true
		}
		if s.Initial && s.Terminal {
			return fmt.Errorf("workflow: state %q cannot be both initial and terminal", name)
		}
	}
	if initials != 1 {
		return fmt.Errorf("workflow: exactly one initial state required, got %d", initials)
	}
	pairs := map[string]bool{}
	for i, t := range wf.Transitions {
		if !seen[t.From] {
			return fmt.Errorf("workflow: transition %d: unknown from state %q", i, t.From)
		}
		if !seen[t.To] {
			return fmt.Errorf("workflow: transition %d: unknown to state %q", i, t.To)
		}
		if t.From == t.To {
			return fmt.Errorf("workflow: transition %d: self-transition %q not allowed", i, t.From)
		}
		if terminals[t.From] {
			return fmt.Errorf("workflow: transition %d: terminal state %q has no outgoing transitions", i, t.From)
		}
		key := t.From + "->" + t.To
		if pairs[key] {
			return fmt.Errorf("workflow: duplicate transition %q", key)
		}
		pairs[key] = true
		if t.Eval != nil {
			if err := t.Eval.Validate(); err != nil {
				return fmt.Errorf("workflow: transition %q: %w", key, err)
			}
		}
	}
	return nil
}

// Validate checks an eval hook declaration.
func (h EvalHook) Validate() error {
	if strings.TrimSpace(h.Name) == "" {
		return fmt.Errorf("eval: name required")
	}
	if strings.TrimSpace(h.Command) == "" {
		return fmt.Errorf("eval %q: command required", h.Name)
	}
	if h.TimeoutSeconds < 0 || h.TimeoutSeconds > MaxEvalTimeoutSeconds {
		return fmt.Errorf("eval %q: timeout_seconds must be 0..%d", h.Name, MaxEvalTimeoutSeconds)
	}
	return nil
}

// Timeout returns the effective timeout for the hook.
func (h EvalHook) Timeout() int {
	if h.TimeoutSeconds <= 0 {
		return DefaultEvalTimeoutSeconds
	}
	return h.TimeoutSeconds
}

// Find returns the transition from->to, or nil.
func (wf Workflow) Find(from, to string) *Transition {
	for i := range wf.Transitions {
		if wf.Transitions[i].From == from && wf.Transitions[i].To == to {
			return &wf.Transitions[i]
		}
	}
	return nil
}

// Available returns the transitions leaving a state.
func (wf Workflow) Available(from string) []Transition {
	var out []Transition
	for _, t := range wf.Transitions {
		if t.From == from {
			out = append(out, t)
		}
	}
	return out
}
