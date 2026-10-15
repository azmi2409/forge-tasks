package workflow

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

const testDef = `{
	"states": [
		{"name": "todo", "initial": true},
		{"name": "doing"},
		{"name": "done", "terminal": true}
	],
	"transitions": [
		{"from": "todo", "to": "doing"},
		{"from": "doing", "to": "todo"},
		{"from": "doing", "to": "done", "require_criteria_met": true,
		 "eval": {"name": "smoke", "command": "true"}}
	]
}`

func mustParse(t *testing.T, def string) Workflow {
	t.Helper()
	wf, err := Parse(json.RawMessage(def))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return wf
}

func TestParseValid(t *testing.T) {
	wf := mustParse(t, testDef)
	if len(wf.States) != 3 || len(wf.Transitions) != 3 {
		t.Fatalf("wf=%+v", wf)
	}
	tr := wf.Find("doing", "done")
	if tr == nil || !tr.RequireCriteriaMet || tr.Eval == nil || tr.Eval.Name != "smoke" {
		t.Fatalf("transition=%+v", tr)
	}
}

func TestParseInvalid(t *testing.T) {
	cases := map[string]string{
		"no states":        `{"states": [], "transitions": []}`,
		"no initial":       `{"states": [{"name": "a"}, {"name": "b", "terminal": true}], "transitions": []}`,
		"two initials":     `{"states": [{"name": "a", "initial": true}, {"name": "b", "initial": true}], "transitions": []}`,
		"dup state":        `{"states": [{"name": "a", "initial": true}, {"name": "a"}], "transitions": []}`,
		"unknown from":     `{"states": [{"name": "a", "initial": true}], "transitions": [{"from": "x", "to": "a"}]}`,
		"terminal outward": `{"states": [{"name": "a", "initial": true}, {"name": "b", "terminal": true}], "transitions": [{"from": "b", "to": "a"}]}`,
		"self transition":  `{"states": [{"name": "a", "initial": true}], "transitions": [{"from": "a", "to": "a"}]}`,
		"bad eval":         `{"states": [{"name": "a", "initial": true}, {"name": "b"}], "transitions": [{"from": "a", "to": "b", "eval": {"name": "x"}}]}`,
		"unknown field":    `{"states": [{"name": "a", "initial": true, "bogus": 1}], "transitions": []}`,
	}
	for name, def := range cases {
		if _, err := Parse(json.RawMessage(def)); err == nil {
			t.Errorf("%s: got nil, want error", name)
		}
	}
}

func TestEvaluate(t *testing.T) {
	wf := mustParse(t, testDef)

	// Plain transition, no preconditions.
	if r := wf.Evaluate("todo", "doing", nil); !r.Allowed {
		t.Fatalf("todo->doing: %+v", r)
	}
	// Unknown transition: machine-readable alternatives.
	r := wf.Evaluate("todo", "done", nil)
	if r.Allowed || r.BlockedCode != "unknown_transition" {
		t.Fatalf("todo->done: %+v", r)
	}
	if len(r.Available) != 1 || r.Available[0] != "doing" {
		t.Fatalf("available=%v", r.Available)
	}
	// Same-state is not a transition.
	if r := wf.Evaluate("doing", "doing", nil); r.BlockedCode != "unknown_transition" {
		t.Fatalf("self: %+v", r)
	}
	// Criteria gate: missing IDs listed.
	crit := []Criterion{{ID: "c1", Satisfied: true}, {ID: "c2", Satisfied: false}}
	r = wf.Evaluate("doing", "done", crit)
	if r.Allowed || r.BlockedCode != "criteria_not_met" {
		t.Fatalf("criteria: %+v", r)
	}
	if len(r.Missing) != 1 || r.Missing[0] != "c2" {
		t.Fatalf("missing=%v", r.Missing)
	}
	// All satisfied -> allowed (eval runs later, at apply time).
	crit = []Criterion{{ID: "c1", Satisfied: true}, {ID: "c2", Satisfied: true}}
	if r := wf.Evaluate("doing", "done", crit); !r.Allowed {
		t.Fatalf("all met: %+v", r)
	}
	// Vacuous truth: no criteria at all still allows the transition.
	if r := wf.Evaluate("doing", "done", nil); !r.Allowed {
		t.Fatalf("empty criteria: %+v", r)
	}
}

func TestAvailable(t *testing.T) {
	wf := mustParse(t, testDef)
	got := wf.Available("doing")
	if len(got) != 2 {
		t.Fatalf("got=%+v", got)
	}
	if len(wf.Available("done")) != 0 {
		t.Fatalf("terminal has outgoing: %+v", wf.Available("done"))
	}
}

func TestRunEvalHook(t *testing.T) {
	ctx := context.Background()
	ectx := EvalContext{WorkItemKey: "FT-1", From: "doing", To: "done"}

	res, err := RunEvalHook(ctx, EvalHook{Name: "pass", Command: "true"}, ectx)
	if err != nil || !res.Passed || res.ExitCode != 0 {
		t.Fatalf("pass: %+v %v", res, err)
	}

	res, err = RunEvalHook(ctx, EvalHook{Name: "fail", Command: "false"}, ectx)
	if err != nil || res.Passed || res.ExitCode != 1 {
		t.Fatalf("fail: %+v %v", res, err)
	}

	// Timeout kills the run and reports it (not an error).
	res, err = RunEvalHook(ctx, EvalHook{Name: "slow", Command: "sleep 30", TimeoutSeconds: 1}, ectx)
	if err != nil || res.Passed || !res.TimedOut {
		t.Fatalf("timeout: %+v %v", res, err)
	}
	if res.Duration > 10*time.Second {
		t.Fatalf("timeout took too long: %v", res.Duration)
	}

	// Context is visible to the check script.
	res, err = RunEvalHook(ctx, EvalHook{Name: "env", Command: `echo "$FORGE_WORK_ITEM_KEY $FORGE_TRANSITION"`}, ectx)
	if err != nil || !res.Passed || strings.TrimSpace(res.Output) != "FT-1 doing->done" {
		t.Fatalf("env: %+v %v", res, err)
	}

	// Output is bounded.
	res, err = RunEvalHook(ctx, EvalHook{Name: "noisy", Command: "yes | head -c 100000"}, ectx)
	if err != nil || !res.Passed {
		t.Fatalf("noisy: %+v %v", res, err)
	}
	if !res.Truncated || len(res.Output) > MaxEvalOutputBytes+10 {
		t.Fatalf("truncation: truncated=%v len=%d", res.Truncated, len(res.Output))
	}

	// Invalid hook is an error, not a failure.
	if _, err := RunEvalHook(ctx, EvalHook{Name: "bad"}, ectx); err == nil {
		t.Fatal("bad hook: got nil, want error")
	}
}
