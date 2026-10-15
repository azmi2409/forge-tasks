package workflow

// Criterion is the workflow engine's minimal view of an acceptance criterion.
// The store's richer type maps onto this at the boundary, keeping the engine
// decoupled from storage.
type Criterion struct {
	ID        string
	Satisfied bool
}

// Result is the machine-readable answer to "can this item move to `to`?"
type Result struct {
	Allowed bool
	// When Allowed, the matched transition.
	Transition *Transition
	// When !Allowed, why not. Codes: "unknown_transition",
	// "criteria_not_met". (Eval failures surface via EvalResult, not here,
	// because evals have side effects and only run on explicit request.)
	BlockedCode string   `json:"blocked_code,omitempty"`
	Message     string   `json:"message,omitempty"`
	Missing     []string `json:"missing,omitempty"`   // criterion IDs not yet satisfied
	Available   []string `json:"available,omitempty"` // legal targets from here
}

// Evaluate checks the transition's declared preconditions as pure functions.
// It never runs eval hooks: those have side effects (shell commands) and
// only run when the caller explicitly requests the transition.
func (wf Workflow) Evaluate(from, to string, criteria []Criterion) Result {
	tr := wf.Find(from, to)
	if tr == nil {
		var avail []string
		for _, a := range wf.Available(from) {
			avail = append(avail, a.To)
		}
		return Result{
			BlockedCode: "unknown_transition",
			Message:     "no transition from " + from + " to " + to,
			Available:   avail,
		}
	}
	if tr.RequireCriteriaMet {
		var missing []string
		for _, c := range criteria {
			if !c.Satisfied {
				missing = append(missing, c.ID)
			}
		}
		if len(missing) > 0 {
			return Result{
				Allowed:     false,
				Transition:  tr,
				BlockedCode: "criteria_not_met",
				Message:     "acceptance criteria not satisfied",
				Missing:     missing,
			}
		}
	}
	return Result{Allowed: true, Transition: tr}
}

// CriteriaOf maps store-style criteria onto the engine's view. Callers in
// the api package adapt their own types; this helper exists for tests.
func CriteriaOf(ids []string, satisfied map[string]bool) []Criterion {
	out := make([]Criterion, 0, len(ids))
	for _, id := range ids {
		out = append(out, Criterion{ID: id, Satisfied: satisfied[id]})
	}
	return out
}
