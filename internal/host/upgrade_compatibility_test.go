package host

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kylebegeman/bedrock/internal/compat"
	"github.com/kylebegeman/bedrock/internal/kernel"
	"github.com/kylebegeman/bedrock/internal/state"
)

func candidateCompatibility() compat.Info {
	return compat.Info{APIVersion: 1, ManifestVersion: 1, StateVersion: 1, MinStateVersion: 0, MaxStateVersion: 1, Capabilities: []string{"kernel.plan.v1", "kernel.apply.v1", "kernel.expect.v1", "state.migrations.v1", "upgrade.checkpoint.v1"}}
}
func upgradeCompatibilityFixture(t *testing.T) (*fakeMachine, *state.Store, Upgrade) {
	m := setUpBox(t)
	allowEverything(m)
	m.write(BinaryPath, "old binary")
	m.write("/tmp/candidate", "new binary")
	m.answers["/tmp/candidate version --json"] = `{"version":"0.8.0-test","commit":"bbbbbbbbbbbb","os":"linux","arch":"amd64"}`
	info := candidateCompatibility()
	body, _ := json.Marshal(info)
	m.answers["/tmp/candidate capabilities --json"] = string(body)
	store := openStore(t)
	return m, store, Upgrade{Env: m.env(), Store: store}
}
func TestUpgradeRejectsIncompatibleCandidateBeforeJournalOrBinaryChanges(t *testing.T) {
	for _, kind := range []string{"downgrade", "new-schema", "api", "missing-capability"} {
		t.Run(kind, func(t *testing.T) {
			m, store, u := upgradeCompatibilityFixture(t)
			info := candidateCompatibility()
			switch kind {
			case "downgrade":
				info.StateVersion = 0
				info.MaxStateVersion = 0
			case "new-schema":
				info.StateVersion = 2
				info.MaxStateVersion = 2
			case "api":
				info.APIVersion = 2
			case "missing-capability":
				info.Capabilities = nil
			}
			body, _ := json.Marshal(info)
			m.answers["/tmp/candidate capabilities --json"] = string(body)
			reg := kernel.Registry{}
			reg.Add(u)
			engine := kernel.New(store, reg, "test")
			if _, err := engine.Run(context.Background(), UpgradeKind, upgradeInput(t, "/tmp/candidate"), func(kernel.Event) {}); err == nil || !strings.Contains(err.Error(), "preflight") {
				t.Fatalf("%v", err)
			}
			ops, _ := store.List(context.Background(), 10)
			if len(ops) != 0 || m.read(BinaryPath) != "old binary" || m.read(PreviousBinary) != "" {
				t.Fatalf("rejection wrote state or binary: %+v", ops)
			}
		})
	}
}
func TestUpgradeCheckpointsConsistentPrivateStateBeforeSwitch(t *testing.T) {
	m, store, u := upgradeCompatibilityFixture(t)
	ctx := context.Background()
	store.AddWatch(ctx, "https://before.example.com", time.Now())
	plan, err := u.Plan(ctx, upgradeInput(t, "/tmp/candidate"))
	if err != nil {
		t.Fatal(err)
	}
	var output strings.Builder
	if err := plan.Steps[0].Apply(ctx, &output); err != nil {
		t.Fatal(err)
	}
	var metadata upgradeCheckpoint
	if err := json.Unmarshal([]byte(m.read(LibDir+"/state-checkpoint.json")), &metadata); err != nil {
		t.Fatal(err)
	}
	saved, err := state.OpenReadOnly(metadata.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer saved.Close()
	watches, err := saved.Watches(ctx)
	if err != nil || len(watches) != 1 || metadata.StateVersion != state.SchemaVersion {
		t.Fatalf("%+v %+v %v", metadata, watches, err)
	}
	info, _ := os.Stat(metadata.Path)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("checkpoint is public: %v", info.Mode())
	}
	if m.read(BinaryPath) != "old binary" || m.read(PreviousBinary) != "old binary" {
		t.Fatal("stage switched the binary")
	}
	if err := plan.Steps[1].Apply(ctx, &output); err != nil {
		t.Fatal(err)
	}
	if m.read(BinaryPath) != "new binary" {
		t.Fatal("switch did not run after checkpoint")
	}
}

func TestUpgradeCheckpointIsBoundToItsJournaledOperation(t *testing.T) {
	m, store, u := upgradeCompatibilityFixture(t)
	reg := kernel.Registry{}
	reg.Add(u)
	engine := kernel.New(store, reg, "test")
	receipt, err := engine.Run(context.Background(), UpgradeKind, upgradeInput(t, "/tmp/candidate"), func(kernel.Event) {})
	if err != nil {
		t.Fatal(err)
	}
	var checkpoint upgradeCheckpoint
	if err := json.Unmarshal([]byte(m.read(LibDir+"/state-checkpoint.json")), &checkpoint); err != nil {
		t.Fatal(err)
	}
	if checkpoint.OperationID != receipt.ID {
		t.Fatalf("checkpoint operation %s receipt %s", checkpoint.OperationID, receipt.ID)
	}
	saved, err := state.OpenReadOnly(checkpoint.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer saved.Close()
	operation, steps, err := saved.Get(context.Background(), receipt.ID)
	if err != nil || operation.Status != state.Applying || len(steps) != 4 || steps[0].Status != state.StepRunning {
		t.Fatalf("%+v %+v %v", operation, steps, err)
	}
}
