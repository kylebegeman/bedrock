package api

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/kylebegeman/bedrock/internal/kernel"
	"github.com/kylebegeman/bedrock/internal/state"
)

func TestDaemonAndFallbackShareMachineOperationLock(t *testing.T) {
	store, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	reg := kernel.Registry{}
	reg.Add(kernel.Exercise{})
	engine := kernel.New(store, reg, "daemon")
	server := &Server{Store: store, Engine: engine}
	unlock, err := lockOperations(context.Background(), store.Path())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	raw, _ := json.Marshal(kernel.ExerciseInput{Dir: filepath.Join(t.TempDir(), "markers"), Steps: 1})
	if _, err := server.RunLocked(ctx, kernel.ExerciseKind, raw, func(kernel.Event) {}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("daemon bypassed fallback lock: %v", err)
	}
	ops, err := store.List(context.Background(), 10)
	if err != nil || len(ops) != 0 {
		t.Fatalf("blocked operation journaled: %v %v", ops, err)
	}
	unlock()
	receipt, err := server.RunLocked(context.Background(), kernel.ExerciseKind, raw, func(kernel.Event) {})
	if err != nil || receipt.Status != "succeeded" {
		t.Fatalf("lock not released: %+v %v", receipt, err)
	}
}

func TestQueuedDaemonWorkHonorsCancellation(t *testing.T) {
	for _, kind := range []string{"run", "recover"} {
		t.Run(kind, func(t *testing.T) {
			server := &Server{}
			server.mu.Lock()
			defer server.mu.Unlock()
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			done := make(chan error, 1)
			go func() {
				if kind == "recover" {
					_, err := server.Recover(ctx, func(kernel.Event) {})
					done <- err
				} else {
					_, err := server.RunLocked(ctx, "test", nil, func(kernel.Event) {})
					done <- err
				}
			}()
			select {
			case err := <-done:
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("cancellation: %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("queued work ignored cancellation")
			}
		})
	}
}
