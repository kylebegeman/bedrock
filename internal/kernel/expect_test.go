package kernel

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/kylebegeman/bedrock/internal/state"
)

// Reading a plan and applying it are two acts, and the point of naming the
// digest is that nothing may change in between.
func TestRunExpectingAppliesTheNamedPlan(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	e := New(newStore(t), registry(), "test-expect")

	view, err := e.PlanOnly(ctx, ExerciseKind, exercise(t, dir, 2, 0, Resume))
	if err != nil {
		t.Fatal(err)
	}
	if view.Digest == "" {
		t.Fatal("a plan has to have a digest to name")
	}
	receipt, err := e.RunExpecting(ctx, ExerciseKind, exercise(t, dir, 2, 0, Resume), view.Digest, func(Event) {})
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Status != state.Succeeded || receipt.PlanDigest != view.Digest {
		t.Fatalf("receipt: %+v", receipt)
	}
}

// A plan that has moved on must not be applied under the old approval, and
// nothing may run before that is noticed.
func TestRunExpectingRefusesAPlanThatMoved(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store := newStore(t)
	e := New(store, registry(), "test-expect")

	view, err := e.PlanOnly(ctx, ExerciseKind, exercise(t, dir, 2, 0, Resume))
	if err != nil {
		t.Fatal(err)
	}
	// Apply something that is not what was read: three steps, not two.
	_, err = e.RunExpecting(ctx, ExerciseKind, exercise(t, dir, 3, 0, Resume), view.Digest, func(Event) {
		t.Fatal("nothing may be emitted for a plan that is refused")
	})
	if !errors.Is(err, ErrPlanChanged) {
		t.Fatalf("want ErrPlanChanged, got %v", err)
	}
	for n := 1; n <= 3; n++ {
		if exists(filepath.Join(dir, "step-"+string(rune('0'+n)))) {
			t.Fatalf("step-%d ran despite the refusal", n)
		}
	}
	ops, err := store.List(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(ops) != 0 {
		t.Fatalf("a refused plan recorded %d operation(s)", len(ops))
	}
}

// An empty digest is the ordinary path and must keep working.
func TestRunExpectingWithoutADigestAppliesAsBefore(t *testing.T) {
	dir := t.TempDir()
	e := New(newStore(t), registry(), "test-expect")
	receipt, err := e.RunExpecting(context.Background(), ExerciseKind, exercise(t, dir, 1, 0, Resume), "", func(Event) {})
	if err != nil || receipt.Status != state.Succeeded {
		t.Fatalf("%v %+v", err, receipt)
	}
	if _, err := os.Stat(filepath.Join(dir, "step-1")); err != nil {
		t.Fatal("the step should have run")
	}
}
