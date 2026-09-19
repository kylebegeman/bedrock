package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kylebegeman/quark/internal/api"
	"github.com/kylebegeman/quark/internal/kernel"
	"github.com/kylebegeman/quark/internal/state"
)

// TestASweepRecoversAnOperationWhoseLeaseOutlivesTheRestart is the lane's
// first failure as a unit test: the daemon is killed, systemd restarts it
// within seconds, and the dead daemon's lease is still live at start, so
// startup recovery sees nothing. The sweep must pick it up once the lease
// expires.
func TestASweepRecoversAnOperationWhoseLeaseOutlivesTheRestart(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stateDir := t.TempDir()
	dir := filepath.Join(stateDir, "markers")
	store, err := state.Open(filepath.Join(stateDir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}

	// A dead daemon left this operation applying with 1.5 s of lease left,
	// after finishing step 1.
	input, _ := json.Marshal(kernel.ExerciseInput{Dir: dir, Steps: 3})
	dead := kernel.New(store, Registry(), "dead-daemon")
	view, err := dead.PlanOnly(ctx, kernel.ExerciseKind, input)
	if err != nil {
		t.Fatal(err)
	}
	planJSON, _ := json.Marshal(view)
	now := time.Now().UTC()
	if err := store.CreateOperation(ctx, state.NewOperation{ID: "op-dead", Kind: kernel.ExerciseKind, Target: dir, Input: input, Plan: planJSON, PlanDigest: view.Digest, Recovery: string(kernel.Resume), StepNames: []string{"step-1", "step-2", "step-3"}, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Claim(ctx, "op-dead", "dead-daemon", now, now.Add(1500*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if err := store.StepStarted(ctx, "op-dead", 0, now); err != nil {
		t.Fatal(err)
	}
	if err := store.StepFinished(ctx, "op-dead", 0, state.StepSucceeded, "", "", now); err != nil {
		t.Fatal(err)
	}
	store.Close()

	socketDir, err := os.MkdirTemp("/tmp", "quark-daemon-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(socketDir)
	socket := filepath.Join(socketDir, "q.sock")
	var log bytes.Buffer
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Config{StateDir: stateDir, Socket: socket, Owner: "new-daemon", SweepInterval: 200 * time.Millisecond}, &log)
	}()

	client := api.Dial(socket)
	defer client.Close()
	deadline := time.Now().Add(10 * time.Second)
	for {
		receipt, err := client.Receipt(ctx, "op-dead")
		if err == nil && receipt.Status == state.Succeeded {
			if receipt.Interruptions != 1 || receipt.Owner != "new-daemon" || receipt.Steps[0].Attempts != 1 {
				t.Fatalf("receipt: %+v", receipt)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("not recovered in time; receipt=%+v err=%v log=%s", receipt, err, log.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
	if _, err := os.Stat(filepath.Join(dir, "step-3")); err != nil {
		t.Fatal("the resumed operation didn't finish its steps")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("daemon: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("daemon didn't stop")
	}
	if !bytes.Contains(log.Bytes(), []byte("recovering op-dead")) {
		t.Fatalf("log: %s", log.String())
	}
}
