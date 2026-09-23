package api

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kylebegeman/bedrock/internal/kernel"
	"github.com/kylebegeman/bedrock/internal/state"
)

func server(t *testing.T) (*Client, *state.Store) {
	t.Helper()
	store, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	reg := kernel.Registry{}
	reg.Add(kernel.Exercise{})
	srv := &Server{Engine: kernel.New(store, reg, "test-daemon"), Store: store}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return NewClient(ts.Client(), ts.URL), store
}

func input(t *testing.T, dir string, steps, failAt int) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(kernel.ExerciseInput{Dir: dir, Steps: steps, FailAt: failAt})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestRunStreamsEventsAndEndsWithTheReceipt(t *testing.T) {
	ctx := context.Background()
	client, _ := server(t)
	dir := t.TempDir()
	var seen []kernel.EventType
	receipt, err := client.Run(ctx, kernel.ExerciseKind, input(t, dir, 2, 0), func(ev kernel.Event) { seen = append(seen, ev.Type) })
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Status != state.Succeeded || len(receipt.Steps) != 2 {
		t.Fatalf("receipt: %+v", receipt)
	}
	if seen[0] != kernel.EventPlanned || seen[len(seen)-1] != kernel.EventFinished {
		t.Fatalf("events: %v", seen)
	}
	if _, err := os.Stat(filepath.Join(dir, "step-2")); err != nil {
		t.Fatal("the operation didn't run on the server side")
	}
	rows, err := client.List(ctx, 10)
	if err != nil || len(rows) != 1 || rows[0].ID != receipt.ID || rows[0].Status != state.Succeeded {
		t.Fatalf("list: %v %+v", err, rows)
	}
	again, err := client.Receipt(ctx, receipt.ID)
	if err != nil || again.PlanDigest != receipt.PlanDigest {
		t.Fatalf("receipt by id: %v %+v", err, again)
	}
	info, err := client.Version(ctx)
	if err != nil || info.Version == "" {
		t.Fatalf("version: %v %+v", err, info)
	}
}

func TestPlanOnlyChangesNothing(t *testing.T) {
	ctx := context.Background()
	client, store := server(t)
	dir := t.TempDir()
	view, err := client.Plan(ctx, kernel.ExerciseKind, input(t, dir, 3, 0))
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Steps) != 3 || view.Digest == "" {
		t.Fatalf("plan: %+v", view)
	}
	if _, err := os.Stat(filepath.Join(dir, "step-1")); err == nil {
		t.Fatal("planning wrote a marker")
	}
	if ops, _ := store.List(ctx, 10); len(ops) != 0 {
		t.Fatal("planning recorded an operation")
	}
}

func TestErrorsComeBackAsErrors(t *testing.T) {
	ctx := context.Background()
	client, _ := server(t)
	if _, err := client.Plan(ctx, "no.such.kind", json.RawMessage(`{}`)); err == nil || err.Error() != `unknown operation kind "no.such.kind"` {
		t.Fatalf("unknown kind: %v", err)
	}
	if _, err := client.Receipt(ctx, "op-nope"); err == nil || err.Error() != "not found" {
		t.Fatalf("missing receipt: %v", err)
	}
	if _, err := client.Run(ctx, kernel.ExerciseKind, json.RawMessage(`{"dir":""}`), func(kernel.Event) {}); err == nil {
		t.Fatal("a bad input must fail the run")
	}
}

func TestListenReplacesAStaleSocket(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "q.sock")
	if err := os.WriteFile(socket, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	l, err := Listen(socket)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	info, err := os.Stat(socket)
	if err != nil || info.Mode().Perm() != 0o660 {
		t.Fatalf("socket mode: %v %v", err, info.Mode())
	}
	if _, err := Listen(socket); err == nil {
		t.Fatal("a live socket must not be replaced")
	}
}

func TestOnlyOneLocalOperationRunsAtATime(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	unlock, err := lockOperations(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, err := lockOperations(ctx, path); err == nil || !strings.Contains(err.Error(), "another bedrock operation is running on this machine") {
		t.Fatalf("a second operation got the lock: %v", err)
	}
	unlock()
	again, err := lockOperations(context.Background(), path)
	if err != nil {
		t.Fatalf("the lock was not let go: %v", err)
	}
	again()
}
