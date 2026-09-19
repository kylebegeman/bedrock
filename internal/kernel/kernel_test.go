package kernel

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kylebegeman/quark/internal/state"
)

func newStore(t *testing.T) *state.Store {
	t.Helper()
	s, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func registry() Registry {
	r := Registry{}
	r.Add(Exercise{})
	return r
}

func exercise(t *testing.T, dir string, steps, failAt int, recovery Recovery) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(ExerciseInput{Dir: dir, Steps: steps, FailAt: failAt, Recovery: recovery})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

type recorder struct{ events []Event }

func (r *recorder) emit(e Event) { r.events = append(r.events, e) }

func (r *recorder) types() []EventType {
	var out []EventType
	for _, e := range r.events {
		if e.Type != EventStepOutput {
			out = append(out, e.Type)
		}
	}
	return out
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func TestRunAppliesEveryStepAndLeavesAReceipt(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	e := New(newStore(t), registry(), "test-a")
	var rec recorder
	receipt, err := e.Run(ctx, ExerciseKind, exercise(t, dir, 3, 0, Resume), rec.emit)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Status != state.Succeeded || len(receipt.Steps) != 3 || receipt.Interruptions != 0 {
		t.Fatalf("receipt: %+v", receipt)
	}
	for n := 1; n <= 3; n++ {
		if !exists(filepath.Join(dir, "step-"+string(rune('0'+n)))) {
			t.Fatalf("step-%d marker missing", n)
		}
	}
	want := []EventType{EventPlanned, EventStepStarted, EventStepFinished, EventStepStarted, EventStepFinished, EventStepStarted, EventStepFinished, EventFinished}
	if got := rec.types(); len(got) != len(want) {
		t.Fatalf("events %v, want %v", got, want)
	}
	if rec.events[0].Plan == nil || rec.events[0].Plan.Digest == "" || len(rec.events[0].Plan.Steps) != 3 {
		t.Fatalf("planned event: %+v", rec.events[0])
	}
	stored, err := e.ReceiptOf(ctx, receipt.ID)
	if err != nil || stored.Status != state.Succeeded || stored.PlanDigest != receipt.PlanDigest {
		t.Fatalf("stored receipt: %v %+v", err, stored)
	}
}

func TestPlanOnlyRecordsNothing(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store := newStore(t)
	e := New(store, registry(), "test-a")
	view, err := e.PlanOnly(ctx, ExerciseKind, exercise(t, dir, 2, 0, Resume))
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Steps) != 2 || view.Steps[1].Name != "step-2" || view.Recovery != Resume {
		t.Fatalf("plan: %+v", view)
	}
	if exists(filepath.Join(dir, "step-1")) {
		t.Fatal("planning must change nothing")
	}
	if ops, _ := store.List(ctx, 10); len(ops) != 0 {
		t.Fatalf("planning must record nothing, got %d operations", len(ops))
	}
}

func TestFailureWithoutUndoStopsAndFails(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	e := New(newStore(t), registry(), "test-a")
	var rec recorder
	receipt, err := e.Run(ctx, ExerciseKind, exercise(t, dir, 3, 2, Resume), rec.emit)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Status != state.Failed || receipt.Error != "step-2: asked to fail at step 2" {
		t.Fatalf("receipt: %+v", receipt)
	}
	if !exists(filepath.Join(dir, "step-1")) || exists(filepath.Join(dir, "step-3")) {
		t.Fatal("step 1 should stay, step 3 should never run")
	}
	if receipt.Steps[1].Status != state.StepFailed || receipt.Steps[2].Status != state.StepPending {
		t.Fatalf("steps: %+v", receipt.Steps)
	}
}

func TestFailureWithCompensationUndoesTheRest(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	e := New(newStore(t), registry(), "test-a")
	var rec recorder
	receipt, err := e.Run(ctx, ExerciseKind, exercise(t, dir, 3, 3, Compensate), rec.emit)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Status != state.Compensated {
		t.Fatalf("receipt: %+v", receipt)
	}
	if exists(filepath.Join(dir, "step-1")) || exists(filepath.Join(dir, "step-2")) {
		t.Fatal("completed steps must be undone")
	}
	if receipt.Steps[0].Status != state.StepUndone || receipt.Steps[1].Status != state.StepUndone || receipt.Steps[2].Status != state.StepFailed {
		t.Fatalf("steps: %+v", receipt.Steps)
	}
	// Undo events run in reverse order and say so.
	var undone []string
	for _, ev := range rec.events {
		if ev.Type == EventStepFinished && ev.Step.Undoing {
			undone = append(undone, ev.Step.Name)
		}
	}
	if len(undone) != 2 || undone[0] != "step-2" || undone[1] != "step-1" {
		t.Fatalf("undo order: %v", undone)
	}
}

func TestACrashIsResumedByTheNextDaemon(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store := newStore(t)
	t0 := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	first := New(store, registry(), "daemon-a", WithLeaseTTL(time.Minute), WithClock(func() time.Time { return t0 }))
	first.crashAfterStep = func(i int) bool { return i == 0 }
	var rec recorder
	if _, err := first.Run(ctx, ExerciseKind, exercise(t, dir, 3, 0, Resume), rec.emit); !errors.Is(err, errCrashed) {
		t.Fatalf("want the simulated crash, got %v", err)
	}
	ops, _ := store.List(ctx, 1)
	if len(ops) != 1 || ops[0].Status != state.Applying || ops[0].LeaseOwner != "daemon-a" {
		t.Fatalf("after crash: %+v", ops)
	}

	// While the lease is live, nothing is orphaned.
	early := New(store, registry(), "daemon-b", WithLeaseTTL(time.Minute), WithClock(func() time.Time { return t0.Add(30 * time.Second) }))
	if n, err := early.Recover(ctx, rec.emit); err != nil || n != 0 {
		t.Fatalf("recover during a live lease: n=%d err=%v", n, err)
	}

	// After it expires, the next daemon resumes from step 2.
	later := New(store, registry(), "daemon-b", WithLeaseTTL(time.Minute), WithClock(func() time.Time { return t0.Add(2 * time.Minute) }))
	rec = recorder{}
	n, err := later.Recover(ctx, rec.emit)
	if err != nil || n != 1 {
		t.Fatalf("recover: n=%d err=%v", n, err)
	}
	receipt, err := later.ReceiptOf(ctx, ops[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	// The receipt names the daemon that finished it.
	if receipt.Status != state.Succeeded || receipt.Interruptions != 1 || receipt.Owner != "daemon-b" {
		t.Fatalf("receipt: %+v", receipt)
	}
	if receipt.Steps[0].Attempts != 1 || receipt.Steps[1].Attempts != 1 || receipt.Steps[2].Attempts != 1 {
		t.Fatalf("step 1 must not rerun: %+v", receipt.Steps)
	}
	if got := rec.types(); got[0] != EventResumed || got[len(got)-1] != EventFinished {
		t.Fatalf("events: %v", got)
	}
	for n := 1; n <= 3; n++ {
		if !exists(filepath.Join(dir, "step-"+string(rune('0'+n)))) {
			t.Fatalf("step-%d marker missing after resume", n)
		}
	}
}

func TestACrashCanBeCompensatedInstead(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store := newStore(t)
	t0 := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	first := New(store, registry(), "daemon-a", WithLeaseTTL(time.Minute), WithClock(func() time.Time { return t0 }))
	first.crashAfterStep = func(i int) bool { return i == 1 }
	var rec recorder
	if _, err := first.Run(ctx, ExerciseKind, exercise(t, dir, 3, 0, Compensate), rec.emit); !errors.Is(err, errCrashed) {
		t.Fatalf("want the simulated crash, got %v", err)
	}
	later := New(store, registry(), "daemon-b", WithLeaseTTL(time.Minute), WithClock(func() time.Time { return t0.Add(2 * time.Minute) }))
	if _, err := later.Recover(ctx, rec.emit); err != nil {
		t.Fatal(err)
	}
	ops, _ := store.List(ctx, 1)
	receipt, _ := later.ReceiptOf(ctx, ops[0].ID)
	if receipt.Status != state.Compensated || receipt.Interruptions != 1 {
		t.Fatalf("receipt: %+v", receipt)
	}
	if exists(filepath.Join(dir, "step-1")) || exists(filepath.Join(dir, "step-2")) {
		t.Fatal("steps 1 and 2 must be undone")
	}
	if receipt.Steps[2].Status != state.StepPending {
		t.Fatalf("step 3 never ran: %+v", receipt.Steps[2])
	}
}

type otherPlan struct{}

func (otherPlan) Kind() string { return ExerciseKind }
func (otherPlan) Plan(context.Context, json.RawMessage) (*Plan, error) {
	return &Plan{Target: "elsewhere", Steps: []Step{{Name: "different", Apply: func(context.Context, io.Writer) error { return nil }}}}, nil
}

func TestAChangedPlanIsNotResumed(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store := newStore(t)
	t0 := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	first := New(store, registry(), "daemon-a", WithLeaseTTL(time.Minute), WithClock(func() time.Time { return t0 }))
	first.crashAfterStep = func(i int) bool { return i == 0 }
	if _, err := first.Run(ctx, ExerciseKind, exercise(t, dir, 2, 0, Resume), func(Event) {}); !errors.Is(err, errCrashed) {
		t.Fatalf("want the simulated crash, got %v", err)
	}
	changed := Registry{}
	changed.Add(otherPlan{})
	later := New(store, changed, "daemon-b", WithLeaseTTL(time.Minute), WithClock(func() time.Time { return t0.Add(2 * time.Minute) }))
	if _, err := later.Recover(ctx, func(Event) {}); err != nil {
		t.Fatal(err)
	}
	ops, _ := store.List(ctx, 1)
	if ops[0].Status != state.Failed || ops[0].Error != "interrupted, and the plan changed since it started" {
		t.Fatalf("after recover: %+v", ops[0])
	}
}

func TestIDsSortByTimeAndDiffer(t *testing.T) {
	t0 := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	a, b := NewID(t0), NewID(t0.Add(time.Second))
	if a >= b || len(a) != len("op-20260919-120000-abcde") || a[:19] != "op-20260919-120000-" {
		t.Fatalf("ids %q %q", a, b)
	}
	if NewID(t0) == NewID(t0) {
		t.Fatal("ids at the same instant must differ")
	}
}

type attemptRecorder struct{ attempts []int }

func (attemptRecorder) Kind() string { return "test.attempts" }
func (r *attemptRecorder) Plan(context.Context, json.RawMessage) (*Plan, error) {
	return &Plan{Target: "x", Steps: []Step{{Name: "only", Apply: func(ctx context.Context, _ io.Writer) error {
		r.attempts = append(r.attempts, Attempt(ctx))
		return nil
	}}}}, nil
}

func TestStepsKnowTheirAttempt(t *testing.T) {
	if Attempt(context.Background()) != 0 {
		t.Fatal("no attempt outside a step")
	}
	rec := &attemptRecorder{}
	reg := Registry{}
	reg.Add(rec)
	e := New(newStore(t), reg, "test")
	if _, err := e.Run(context.Background(), "test.attempts", json.RawMessage(`{}`), func(Event) {}); err != nil {
		t.Fatal(err)
	}
	if len(rec.attempts) != 1 || rec.attempts[0] != 1 {
		t.Fatalf("attempts: %v", rec.attempts)
	}
}
