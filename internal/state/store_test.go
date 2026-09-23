package state

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "nested", "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func newOp(id string, at time.Time) NewOperation {
	return NewOperation{
		ID: id, Kind: "test.kind", Target: "here", Input: json.RawMessage(`{"n":1}`),
		Plan: json.RawMessage(`[{"name":"one"},{"name":"two"}]`), PlanDigest: "abc", Recovery: "resume",
		StepNames: []string{"one", "two"}, CreatedAt: at,
	}
}

func TestOperationLifecycleLeavesAReceipt(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	t0 := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	if err := s.CreateOperation(ctx, newOp("op-1", t0)); err != nil {
		t.Fatal(err)
	}
	op, err := s.Claim(ctx, "op-1", "daemon-a", t0, t0.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if op.Status != Applying || op.LeaseOwner != "daemon-a" || op.Interruptions != 0 {
		t.Fatalf("after claim: %+v", op)
	}
	if err := s.StepStarted(ctx, "op-1", 0, t0.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := s.StepFinished(ctx, "op-1", 0, StepSucceeded, "", "did one\n", t0.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := s.Finish(ctx, "op-1", Succeeded, "", json.RawMessage(`{"ok":true}`), t0.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	got, steps, err := s.Get(ctx, "op-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != Succeeded || got.LeaseOwner != "" || string(got.Receipt) != `{"ok":true}` || got.FinishedAt != t0.Add(3*time.Second) {
		t.Fatalf("after finish: %+v", got)
	}
	if len(steps) != 2 || steps[0].Status != StepSucceeded || steps[0].Attempts != 1 || steps[0].Output != "did one\n" || steps[1].Status != StepPending {
		t.Fatalf("steps: %+v", steps)
	}
	if _, err := s.Claim(ctx, "op-1", "daemon-b", t0, t0.Add(time.Minute)); err == nil {
		t.Fatal("a finished operation must not be claimable")
	}
}

func TestLeasesGuardAgainstTwoOwners(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	t0 := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	if err := s.CreateOperation(ctx, newOp("op-1", t0)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Claim(ctx, "op-1", "daemon-a", t0, t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Claim(ctx, "op-1", "daemon-b", t0.Add(time.Second), t0.Add(2*time.Minute)); !errors.Is(err, ErrLeased) {
		t.Fatalf("live lease must refuse another owner, got %v", err)
	}
	// The same owner may re-claim (a renewal by another name).
	if _, err := s.Claim(ctx, "op-1", "daemon-a", t0.Add(time.Second), t0.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	// Once the lease expires, the operation is orphaned and a new owner
	// takes it, and that counts as an interruption.
	orphans, err := s.Orphaned(ctx, t0.Add(3*time.Minute))
	if err != nil || len(orphans) != 1 || orphans[0].ID != "op-1" {
		t.Fatalf("orphans: %v %+v", err, orphans)
	}
	op, err := s.Claim(ctx, "op-1", "daemon-b", t0.Add(3*time.Minute), t0.Add(4*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if op.Interruptions != 1 || op.LeaseOwner != "daemon-b" || op.StartedAt != t0 {
		t.Fatalf("after takeover: %+v", op)
	}
	if err := s.Renew(ctx, "op-1", "daemon-a", t0.Add(5*time.Minute)); !errors.Is(err, ErrLeased) {
		t.Fatalf("the old owner must not renew, got %v", err)
	}
	if err := s.Renew(ctx, "op-1", "daemon-b", t0.Add(5*time.Minute)); err != nil {
		t.Fatal(err)
	}
}

func TestListIsNewestFirstAndBounded(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	t0 := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	for i, id := range []string{"op-1", "op-2", "op-3"} {
		if err := s.CreateOperation(ctx, newOp(id, t0.Add(time.Duration(i)*time.Minute))); err != nil {
			t.Fatal(err)
		}
	}
	ops, err := s.List(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(ops) != 2 || ops[0].ID != "op-3" || ops[1].ID != "op-2" {
		t.Fatalf("list: %+v", ops)
	}
	if _, _, err := s.Get(ctx, "op-9"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestReopenKeepsTheJournal(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateOperation(ctx, newOp("op-1", time.Now())); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, steps, err := s.Get(ctx, "op-1"); err != nil || len(steps) != 2 {
		t.Fatalf("after reopen: %v, %d steps", err, len(steps))
	}
}

func TestAPathAURIWouldMisreadStillOpensWhereItWasAsked(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "odd?dir#x%y")
	s, err := Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var mode string
	if err := s.db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil || mode != "wal" {
		t.Fatalf("journal mode %q, %v: the pragmas after the path were lost", mode, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "state.db")); err != nil {
		t.Fatalf("the database is not where it was asked to be: %v", err)
	}
}
