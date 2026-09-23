// Package kernel runs operations. An operation is planned as steps,
// journaled in the state store under a lease while it applies, and finished
// with a receipt. When the daemon dies mid-way, the next daemon resumes the
// remaining steps or undoes the completed ones, as the plan says.
package kernel

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/kylebegeman/bedrock/internal/state"
	"github.com/kylebegeman/bedrock/internal/version"
)

// Step is one unit of work in a plan.
type Step struct {
	// Name identifies the step within its operation. It must be the same
	// each time the same input is planned, because recovery matches by name.
	Name string
	// Change says what the step does, for plans and receipts.
	Change string
	// Note says what the planner saw, such as "already installed". It is
	// shown in plans and left out of the plan digest, because the world can
	// change between planning and a resume without changing the plan.
	Note string
	// Apply does the work. After a crash it may run again, so it must be
	// safe to repeat.
	Apply func(ctx context.Context, out io.Writer) error
	// Undo reverses a step that succeeded. Nil when there is nothing to undo.
	Undo func(ctx context.Context, out io.Writer) error
}

// Recovery says what to do with an operation whose daemon died mid-way.
type Recovery string

const (
	// Resume runs the steps that haven't succeeded yet.
	Resume Recovery = "resume"
	// Compensate undoes the steps that succeeded, in reverse, and marks the
	// operation compensated.
	Compensate Recovery = "compensate"
)

// Plan is what a Definition produces for an input.
type Plan struct {
	Target   string
	Steps    []Step
	Recovery Recovery
}

// Definition is an operation kind: how to plan it from its input.
type Definition interface {
	Kind() string
	// Plan builds the steps for input. It reads the world and changes
	// nothing. For the same input and world it yields the same step names.
	Plan(ctx context.Context, input json.RawMessage) (*Plan, error)
}

// Registry maps kinds to definitions.
type Registry map[string]Definition

// Add registers a definition under its kind.
func (r Registry) Add(d Definition) { r[d.Kind()] = d }

// EventType names what happened.
type EventType string

const (
	EventPlanned      EventType = "planned"
	EventResumed      EventType = "resumed"
	EventStepStarted  EventType = "step-started"
	EventStepOutput   EventType = "step-output"
	EventStepFinished EventType = "step-finished"
	EventFinished     EventType = "finished"
	// EventError is sent by the API when a run could not start or continue.
	EventError EventType = "error"
)

// Event is one thing the kernel reports while an operation runs.
type Event struct {
	Type      EventType `json:"type"`
	Operation string    `json:"operation"`
	At        time.Time `json:"at"`
	Plan      *PlanView `json:"plan,omitempty"`
	Step      *StepView `json:"step,omitempty"`
	Line      string    `json:"line,omitempty"`
	Message   string    `json:"message,omitempty"`
	Receipt   *Receipt  `json:"receipt,omitempty"`
}

// PlanView is a plan as shown to people and recorded in the journal.
type PlanView struct {
	Kind     string         `json:"kind"`
	Target   string         `json:"target"`
	Recovery Recovery       `json:"recovery"`
	Steps    []PlanStepView `json:"steps"`
	Digest   string         `json:"digest"`
}

// PlanStepView is one step of a PlanView.
type PlanStepView struct {
	Index  int    `json:"index"`
	Name   string `json:"name"`
	Change string `json:"change"`
	Note   string `json:"note,omitempty"`
}

// StepView is a step's state as reported in events and receipts.
type StepView struct {
	Index    int              `json:"index"`
	Name     string           `json:"name"`
	Change   string           `json:"change,omitempty"`
	Note     string           `json:"note,omitempty"`
	Status   state.StepStatus `json:"status"`
	Attempts int              `json:"attempts,omitempty"`
	Error    string           `json:"error,omitempty"`
	Duration time.Duration    `json:"duration,omitempty"`
	Undoing  bool             `json:"undoing,omitempty"`
}

// Receipt is the record an operation leaves behind.
type Receipt struct {
	ID            string                `json:"id"`
	Kind          string                `json:"kind"`
	Target        string                `json:"target"`
	Status        state.OperationStatus `json:"status"`
	Error         string                `json:"error,omitempty"`
	Owner         string                `json:"owner"`
	Interruptions int                   `json:"interruptions"`
	StartedAt     time.Time             `json:"started_at"`
	FinishedAt    time.Time             `json:"finished_at"`
	Duration      time.Duration         `json:"duration"`
	InputDigest   string                `json:"input_digest"`
	PlanDigest    string                `json:"plan_digest"`
	Steps         []StepView            `json:"steps"`
	Version       string                `json:"bedrock"`
}

// Engine plans and applies operations against one store.
type Engine struct {
	store    *state.Store
	registry Registry
	owner    string
	leaseTTL time.Duration
	now      func() time.Time
	// crashAfterStep is a test hook. When it returns true the run stops as
	// a killed process would: the lease stays, nothing is finished.
	crashAfterStep func(index int) bool
}

// Option configures an Engine.
type Option func(*Engine)

// WithLeaseTTL sets how long a lease lasts without renewal.
func WithLeaseTTL(d time.Duration) Option { return func(e *Engine) { e.leaseTTL = d } }

// WithClock sets the engine's clock.
func WithClock(now func() time.Time) Option { return func(e *Engine) { e.now = now } }

// DefaultLeaseTTL is how long a lease lasts without renewal. The engine
// renews every third of it, so a dead daemon is noticed within this long.
const DefaultLeaseTTL = 15 * time.Second

// New returns an engine owned by owner, which names the process holding
// leases.
func New(store *state.Store, registry Registry, owner string, opts ...Option) *Engine {
	e := &Engine{store: store, registry: registry, owner: owner, leaseTTL: DefaultLeaseTTL, now: func() time.Time { return time.Now().UTC() }}
	for _, opt := range opts {
		opt(e)
	}
	return e
}

// PlanOnly builds and shows a plan without recording or applying anything.
func (e *Engine) PlanOnly(ctx context.Context, kind string, input json.RawMessage) (*PlanView, error) {
	_, view, err := e.plan(ctx, kind, input)
	return view, err
}

func (e *Engine) plan(ctx context.Context, kind string, input json.RawMessage) (*Plan, *PlanView, error) {
	def, ok := e.registry[kind]
	if !ok {
		return nil, nil, fmt.Errorf("unknown operation kind %q", kind)
	}
	plan, err := def.Plan(ctx, input)
	if err != nil {
		return nil, nil, fmt.Errorf("plan %s: %w", kind, err)
	}
	if len(plan.Steps) == 0 {
		return nil, nil, fmt.Errorf("plan %s: no steps", kind)
	}
	if plan.Recovery == "" {
		plan.Recovery = Resume
	}
	seen := map[string]bool{}
	view := &PlanView{Kind: kind, Target: plan.Target, Recovery: plan.Recovery}
	for i, st := range plan.Steps {
		if st.Name == "" || st.Apply == nil {
			return nil, nil, fmt.Errorf("plan %s: step %d needs a name and an Apply", kind, i)
		}
		if seen[st.Name] {
			return nil, nil, fmt.Errorf("plan %s: step name %q repeats", kind, st.Name)
		}
		seen[st.Name] = true
		view.Steps = append(view.Steps, PlanStepView{Index: i, Name: st.Name, Change: st.Change, Note: st.Note})
	}
	view.Digest = digestPlan(view)
	return plan, view, nil
}

// Run plans, records and applies an operation, reporting events as it
// goes, and returns the receipt. The receipt's Status says how it ended;
// the error is set only when the operation didn't reach a final status.
func (e *Engine) Run(ctx context.Context, kind string, input json.RawMessage, emit func(Event)) (*Receipt, error) {
	return e.RunExpecting(ctx, kind, input, "", emit)
}

// ErrPlanChanged is returned when a caller named the plan it meant to apply
// and the plan built now is a different one.
var ErrPlanChanged = errors.New("the plan changed since it was read")

// RunExpecting is Run, refusing unless the plan it builds has the digest the
// caller names. An empty digest applies whatever the plan turns out to be.
//
// This is what makes it safe to read a plan, show it to someone, and apply
// it as a separate act: between those two moments the manifest, the machine
// or another operation can move, and the plan that would run is then not the
// plan that was approved. The digest is checked here rather than in the
// caller so that it holds for the daemon's API too, not only the CLI.
func (e *Engine) RunExpecting(ctx context.Context, kind string, input json.RawMessage, expect string, emit func(Event)) (*Receipt, error) {
	plan, view, err := e.plan(ctx, kind, input)
	if err != nil {
		return nil, err
	}
	if expect != "" && view.Digest != expect {
		return nil, fmt.Errorf("%w: it is now %s, not %s; read it again", ErrPlanChanged, view.Digest, expect)
	}
	planJSON, err := json.Marshal(view)
	if err != nil {
		return nil, err
	}
	now := e.now()
	id := NewID(now)
	names := make([]string, len(plan.Steps))
	for i, st := range plan.Steps {
		names[i] = st.Name
	}
	if input == nil {
		input = json.RawMessage(`null`)
	}
	err = e.store.CreateOperation(ctx, state.NewOperation{
		ID: id, Kind: kind, Target: plan.Target, Input: input, Plan: planJSON, PlanDigest: view.Digest,
		Recovery: string(plan.Recovery), StepNames: names, CreatedAt: now,
	})
	if err != nil {
		return nil, err
	}
	emit(Event{Type: EventPlanned, Operation: id, At: now, Plan: view})
	op, err := e.store.Claim(ctx, id, e.owner, now, now.Add(e.leaseTTL))
	if err != nil {
		return nil, err
	}
	return e.apply(ctx, op, plan, view, emit)
}

// Recover finds operations left applying by a daemon that died and finishes
// them as their plan asks. It returns how many it handled; one another
// owner claimed first is left to that owner and not counted.
func (e *Engine) Recover(ctx context.Context, emit func(Event)) (int, error) {
	orphans, err := e.store.Orphaned(ctx, e.now())
	if err != nil {
		return 0, err
	}
	handled := 0
	for _, orphan := range orphans {
		now := e.now()
		op, err := e.store.Claim(ctx, orphan.ID, e.owner, now, now.Add(e.leaseTTL))
		if errors.Is(err, state.ErrLeased) {
			continue // someone else got there first
		}
		if err != nil {
			return 0, err
		}
		handled++
		plan, view, planErr := e.plan(ctx, op.Kind, op.Input)
		switch {
		case planErr != nil:
			if err := e.finish(ctx, op.ID, state.Failed, "interrupted, and the plan can't be rebuilt: "+planErr.Error(), emit); err != nil {
				return 0, err
			}
			continue
		case view.Digest != op.PlanDigest:
			if err := e.finish(ctx, op.ID, state.Failed, "interrupted, and the plan changed since it started", emit); err != nil {
				return 0, err
			}
			continue
		}
		emit(Event{Type: EventResumed, Operation: op.ID, At: now, Plan: view,
			Message: fmt.Sprintf("interrupted %d time(s); %s", op.Interruptions, describeRecovery(plan.Recovery))})
		if plan.Recovery == Compensate {
			if _, err := e.compensate(ctx, op, plan, "interrupted before it finished", emit); err != nil {
				return 0, err
			}
			continue
		}
		if _, err := e.apply(ctx, op, plan, view, emit); err != nil && !errors.Is(err, errCrashed) {
			return 0, err
		}
	}
	return handled, nil
}

func describeRecovery(r Recovery) string {
	if r == Compensate {
		return "undoing the completed steps"
	}
	return "resuming the remaining steps"
}

// errCrashed is what the test hook makes apply return.
var errCrashed = errors.New("simulated crash")

type attemptKey struct{}

// Attempt is which attempt of its step the calling Apply is: 1 the first
// time, 2 when a resumed operation runs the step again. Zero outside a
// step.
func Attempt(ctx context.Context) int {
	n, _ := ctx.Value(attemptKey{}).(int)
	return n
}

// apply runs the steps that haven't succeeded, under a lease it keeps renewing.
func (e *Engine) apply(ctx context.Context, op *state.Operation, plan *Plan, view *PlanView, emit func(Event)) (*Receipt, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stopRenewing := e.keepLease(ctx, op.ID, cancel)
	defer stopRenewing()

	steps, err := e.store.Steps(ctx, op.ID)
	if err != nil {
		return nil, err
	}
	if reason := journalFits(op.ID, steps, plan); reason != "" {
		return e.finishWithReceipt(ctx, op.ID, state.Failed, reason, emit)
	}
	for i, st := range plan.Steps {
		if steps[i].Status == state.StepSucceeded {
			continue
		}
		started := e.now()
		if err := e.store.StepStarted(ctx, op.ID, i, started); err != nil {
			return nil, err
		}
		emit(Event{Type: EventStepStarted, Operation: op.ID, At: started, Step: &StepView{Index: i, Name: st.Name, Change: st.Change, Status: state.StepRunning}})
		out := newLineWriter(func(line string) {
			emit(Event{Type: EventStepOutput, Operation: op.ID, At: e.now(), Step: &StepView{Index: i, Name: st.Name}, Line: line})
		})
		stepErr := st.Apply(context.WithValue(ctx, attemptKey{}, steps[i].Attempts+1), out)
		out.Flush()
		finished := e.now()
		status, errText := state.StepSucceeded, ""
		if stepErr != nil {
			status, errText = state.StepFailed, stepErr.Error()
		}
		if err := e.store.StepFinished(ctx, op.ID, i, status, errText, out.Tail(), finished); err != nil {
			return nil, err
		}
		emit(Event{Type: EventStepFinished, Operation: op.ID, At: finished, Step: &StepView{Index: i, Name: st.Name, Change: st.Change, Status: status, Error: errText, Duration: finished.Sub(started)}})
		if stepErr != nil {
			if plan.Recovery == Compensate {
				return e.compensate(ctx, op, plan, fmt.Sprintf("%s: %s", st.Name, stepErr), emit)
			}
			return e.finishWithReceipt(ctx, op.ID, state.Failed, fmt.Sprintf("%s: %s", st.Name, stepErr), emit)
		}
		if e.crashAfterStep != nil && e.crashAfterStep(i) {
			return nil, errCrashed
		}
	}
	return e.finishWithReceipt(ctx, op.ID, state.Succeeded, "", emit)
}

// compensate undoes the succeeded steps in reverse and marks the operation
// compensated with reason.
func (e *Engine) compensate(ctx context.Context, op *state.Operation, plan *Plan, reason string, emit func(Event)) (*Receipt, error) {
	steps, err := e.store.Steps(ctx, op.ID)
	if err != nil {
		return nil, err
	}
	if reason := journalFits(op.ID, steps, plan); reason != "" {
		return e.finishWithReceipt(ctx, op.ID, state.Failed, reason, emit)
	}
	for i := len(plan.Steps) - 1; i >= 0; i-- {
		if steps[i].Status != state.StepSucceeded || plan.Steps[i].Undo == nil {
			continue
		}
		st := plan.Steps[i]
		started := e.now()
		emit(Event{Type: EventStepStarted, Operation: op.ID, At: started, Step: &StepView{Index: i, Name: st.Name, Change: st.Change, Status: state.StepRunning, Undoing: true}})
		out := newLineWriter(func(line string) {
			emit(Event{Type: EventStepOutput, Operation: op.ID, At: e.now(), Step: &StepView{Index: i, Name: st.Name, Undoing: true}, Line: line})
		})
		undoErr := st.Undo(ctx, out)
		out.Flush()
		finished := e.now()
		status, errText := state.StepUndone, ""
		if undoErr != nil {
			status, errText = state.StepFailed, "undo: "+undoErr.Error()
		}
		if err := e.store.StepFinished(ctx, op.ID, i, status, errText, steps[i].Output+out.Tail(), finished); err != nil {
			return nil, err
		}
		emit(Event{Type: EventStepFinished, Operation: op.ID, At: finished, Step: &StepView{Index: i, Name: st.Name, Change: st.Change, Status: status, Error: errText, Duration: finished.Sub(started), Undoing: true}})
		if undoErr != nil {
			return e.finishWithReceipt(ctx, op.ID, state.Failed, fmt.Sprintf("%s; then undoing %s failed: %s", reason, st.Name, undoErr), emit)
		}
	}
	return e.finishWithReceipt(ctx, op.ID, state.Compensated, reason, emit)
}

// journalFits says why an operation's journal can't be applied against a
// plan, or "" when the two describe the same steps. A recovered plan is
// checked by digest before it gets here, so a mismatch means the journal
// itself lost or gained rows; the operation ends rather than the daemon.
func journalFits(id string, steps []state.Step, plan *Plan) string {
	if len(steps) != len(plan.Steps) {
		return fmt.Sprintf("the journal of %s holds %d step(s) for a plan of %d; it can't be applied", id, len(steps), len(plan.Steps))
	}
	for i, st := range steps {
		if st.Name != plan.Steps[i].Name {
			return fmt.Sprintf("the journal of %s names step %d %q where the plan says %q; it can't be applied", id, i+1, st.Name, plan.Steps[i].Name)
		}
	}
	return ""
}

func (e *Engine) finish(ctx context.Context, id string, status state.OperationStatus, reason string, emit func(Event)) error {
	_, err := e.finishWithReceipt(ctx, id, status, reason, emit)
	return err
}

func (e *Engine) finishWithReceipt(ctx context.Context, id string, status state.OperationStatus, reason string, emit func(Event)) (*Receipt, error) {
	now := e.now()
	// Write the receipt from the journal, then store it with the outcome.
	op, steps, err := e.store.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	op.Status, op.Error, op.FinishedAt = status, reason, now
	receipt := buildReceipt(op, steps)
	raw, err := json.Marshal(receipt)
	if err != nil {
		return nil, err
	}
	if err := e.store.Finish(ctx, id, status, reason, raw, now); err != nil {
		return nil, err
	}
	emit(Event{Type: EventFinished, Operation: id, At: now, Receipt: receipt, Message: reason})
	return receipt, nil
}

// keepLease renews the operation's lease until stop is called. Losing the
// lease cancels the run: another owner has taken the operation.
func (e *Engine) keepLease(ctx context.Context, id string, lost context.CancelFunc) (stop func()) {
	interval := e.leaseTTL / 3
	if interval <= 0 {
		interval = time.Second
	}
	done := make(chan struct{})
	var once sync.Once
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := e.store.Renew(ctx, id, e.owner, e.now().Add(e.leaseTTL)); err != nil {
					lost()
					return
				}
			}
		}
	}()
	return func() { once.Do(func() { close(done) }) }
}

// ReceiptOf builds the receipt for a recorded operation, finished or not.
func (e *Engine) ReceiptOf(ctx context.Context, id string) (*Receipt, error) {
	op, steps, err := e.store.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if op.Receipt != nil {
		var r Receipt
		if err := json.Unmarshal(op.Receipt, &r); err == nil {
			return &r, nil
		}
	}
	return buildReceipt(op, steps), nil
}

func buildReceipt(op *state.Operation, steps []state.Step) *Receipt {
	var view PlanView
	_ = json.Unmarshal(op.Plan, &view)
	r := &Receipt{
		ID: op.ID, Kind: op.Kind, Target: op.Target, Status: op.Status, Error: op.Error, Owner: op.LeaseOwner,
		Interruptions: op.Interruptions, StartedAt: op.StartedAt, FinishedAt: op.FinishedAt,
		InputDigest: digestBytes(op.Input), PlanDigest: op.PlanDigest, Version: version.Current().Version,
	}
	if !op.StartedAt.IsZero() && !op.FinishedAt.IsZero() {
		r.Duration = op.FinishedAt.Sub(op.StartedAt)
	}
	for _, st := range steps {
		sv := StepView{Index: st.Index, Name: st.Name, Status: st.Status, Attempts: st.Attempts, Error: st.Error}
		if st.Index < len(view.Steps) {
			sv.Change, sv.Note = view.Steps[st.Index].Change, view.Steps[st.Index].Note
		}
		if !st.StartedAt.IsZero() && !st.FinishedAt.IsZero() {
			sv.Duration = st.FinishedAt.Sub(st.StartedAt)
		}
		r.Steps = append(r.Steps, sv)
	}
	return r
}

// NewID makes an operation ID that sorts by time and can't be guessed.
func NewID(now time.Time) string {
	var b [5]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	const alphabet = "abcdefghijklmnopqrstuvwxyz234567"
	var tail strings.Builder
	for _, c := range b {
		tail.WriteByte(alphabet[int(c)%len(alphabet)])
	}
	return "op-" + now.UTC().Format("20060102-150405") + "-" + tail.String()
}

func digestPlan(view *PlanView) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s\n%s\n%s\n", view.Kind, view.Target, view.Recovery)
	for _, st := range view.Steps {
		fmt.Fprintf(h, "%d\t%s\t%s\n", st.Index, st.Name, st.Change)
	}
	return hex.EncodeToString(h.Sum(nil))[:24]
}

func digestBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:24]
}

// lineWriter turns a step's output into lines and keeps a bounded tail.
type lineWriter struct {
	emit    func(string)
	partial strings.Builder
	tail    []string
}

const tailLines = 40

func newLineWriter(emit func(string)) *lineWriter { return &lineWriter{emit: emit} }

func (w *lineWriter) Write(p []byte) (int, error) {
	for _, c := range p {
		if c == '\n' {
			w.line(w.partial.String())
			w.partial.Reset()
			continue
		}
		w.partial.WriteByte(c)
	}
	return len(p), nil
}

func (w *lineWriter) line(s string) {
	s = strings.TrimRight(s, "\r")
	w.tail = append(w.tail, s)
	if len(w.tail) > tailLines {
		w.tail = w.tail[len(w.tail)-tailLines:]
	}
	w.emit(s)
}

// Flush emits a final line that had no newline.
func (w *lineWriter) Flush() {
	if w.partial.Len() > 0 {
		w.line(w.partial.String())
		w.partial.Reset()
	}
}

// Tail returns the last lines of output, newline-terminated.
func (w *lineWriter) Tail() string {
	if len(w.tail) == 0 {
		return ""
	}
	return strings.Join(w.tail, "\n") + "\n"
}
