package app

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kylebegeman/helios/internal/docker"
	"github.com/kylebegeman/helios/internal/docker/dockertest"
	"github.com/kylebegeman/helios/internal/integration"
	"github.com/kylebegeman/helios/internal/kernel"
	"github.com/kylebegeman/helios/internal/manifest"
	"github.com/kylebegeman/helios/internal/restic"
	"github.com/kylebegeman/helios/internal/state"
)

// Machine preparation runs commands to archive workload images. This fixture
// isolates snapshot and retention recovery, using an already prepared tree.
type backupIdentityDefinition struct{ Backup }

func (b backupIdentityDefinition) Plan(ctx context.Context, raw json.RawMessage) (*kernel.Plan, error) {
	plan, err := b.Backup.Plan(ctx, raw)
	if err != nil {
		return nil, err
	}
	var in BackupInput
	_ = json.Unmarshal(raw, &in)
	if in.App == manifest.ReservedApp {
		storage, err := integration.LoadStorage(b.Secrets)
		if err != nil {
			return nil, err
		}
		target := MachineTarget(b.Hostname())
		x := &backingUp{b: b.Backup, owner: manifest.ReservedApp, target: target, bucket: storage.Bucket(target),
			staging: filepath.Join(b.StateDir, "backups", "machine"), receiptDir: filepath.Join(b.StateDir, "backups", ".receipts"),
			run: &backupRun{store: b.Store, app: manifest.ReservedApp, kind: state.BackupRunBackup}}
		plan.Steps[0].Apply = func(ctx context.Context, _ io.Writer) error {
			if err := x.beginBackup(ctx); err != nil {
				return err
			}
			if err := resetDir(x.staging); err != nil {
				return err
			}
			return writeRecoveryJSON(x.staging+".operation.json", x.receipt)
		}
	}
	return plan, nil
}

func backupIdentityFixture(t *testing.T, machine bool) (backupIdentityDefinition, json.RawMessage) {
	t.Helper()
	r, _ := recoveryFixture(t)
	b := backupIdentityDefinition{Backup{Store: r.Store, Secrets: r.Secrets, StateDir: r.StateDir, Hostname: func() string { return "old-host" }}}
	app := manifest.ReservedApp
	if !machine {
		m := recoveryManifest()
		m.Data = &manifest.Data{Volumes: map[string]manifest.Volume{"uploads": {}}}
		body, _ := json.Marshal(m)
		rev := state.Revision{App: m.App, ID: "active-code", Manifest: body, CreatedAt: time.Now().UTC()}
		if err := r.Store.SaveRevision(context.Background(), rev); err != nil {
			t.Fatal(err)
		}
		if err := r.Store.Activate(context.Background(), m.App, rev.ID, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
		app = m.App
	}
	raw, _ := json.Marshal(BackupInput{App: app})
	return b, raw
}

func TestBackupRecoveryKeepsItsExactSnapshotAfterANewerBackup(t *testing.T) {
	for _, machine := range []bool{false, true} {
		t.Run(map[bool]string{false: "app", true: "machine"}[machine], func(t *testing.T) {
			b, raw := backupIdentityFixture(t, machine)
			at := time.Now().UTC().Truncate(time.Second)
			original := restic.Summary{SnapshotID: strings.Repeat("a", 64), BackupStart: at, BackupEnd: at.Add(time.Second), TotalFiles: 7, TotalBytes: 1234, DataAdded: 100}
			newer := restic.Summary{SnapshotID: strings.Repeat("b", 64), BackupStart: at.Add(time.Minute), BackupEnd: at.Add(time.Minute + time.Second), TotalFiles: 8, TotalBytes: 4567}
			summary := original
			latestCalls := 0
			fake := dockertest.New(t)
			fake.OnCreate = func(c dockertest.Container) {
				fake.Update(c.Name, func(c *dockertest.Container) {
					c.ExitAfter = 1
					switch c.Cmd[0] {
					case "backup":
						body, _ := json.Marshal(struct {
							Type string `json:"message_type"`
							restic.Summary
						}{"summary", summary})
						c.Logs = string(body)
					case "snapshots":
						latestCalls++
						body, _ := json.Marshal([]restic.Snapshot{{ID: newer.SnapshotID, Time: newer.BackupStart, Summary: &newer}})
						c.Logs = string(body)
					case "forget":
						c.Logs = `[]`
					}
				})
			}
			reg := kernel.Registry{}
			reg.Add(b)
			engine := kernel.New(b.Store, reg, "old-daemon", kernel.WithClock(func() time.Time { return at }))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var operation string
			_, err := engine.Run(ctx, BackupKind, raw, func(event kernel.Event) {
				if event.Type == kernel.EventPlanned {
					operation = event.Operation
				}
				if event.Type == kernel.EventStepFinished && event.Step.Name == "snapshot" && event.Step.Status == state.StepSucceeded {
					cancel()
				}
			})
			if err == nil || operation == "" {
				t.Fatalf("backup was not interrupted after capture: %v", err)
			}
			summary = newer
			second := kernel.New(b.Store, reg, "new-backup", kernel.WithClock(func() time.Time { return at.Add(time.Minute) }))
			receipt, err := second.Run(context.Background(), BackupKind, raw, func(kernel.Event) {})
			if err != nil || receipt.Status != state.Succeeded {
				t.Fatalf("newer backup failed: %+v %v", receipt, err)
			}
			recovered := kernel.New(b.Store, reg, "new-daemon", kernel.WithClock(func() time.Time { return at.Add(2 * time.Minute) }))
			if _, err := recovered.Recover(context.Background(), func(kernel.Event) {}); err != nil {
				t.Fatal(err)
			}
			receipt, err = recovered.ReceiptOf(context.Background(), operation)
			if err != nil || receipt.Status != state.Succeeded {
				t.Fatalf("original backup recovery failed: %+v %v", receipt, err)
			}
			runs, err := b.Store.BackupRuns(context.Background(), "", state.BackupRunBackup, 10)
			if err != nil || len(runs) != 2 {
				t.Fatalf("backup operation adopted another operation's run: %+v %v", runs, err)
			}
			var originalRun *state.BackupRun
			for i := range runs {
				if runs[i].Snapshot == original.SnapshotID {
					originalRun = &runs[i]
				}
			}
			if originalRun == nil || !originalRun.OK || originalRun.Files != original.TotalFiles || originalRun.Bytes != original.TotalBytes || !originalRun.SnapshotAt.Equal(original.BackupStart) {
				t.Fatalf("original backup lost its exact capture receipt: %+v", runs)
			}
			if latestCalls != 0 {
				t.Fatalf("recovery resolved repository Latest %d time(s)", latestCalls)
			}
		})
	}
}

func TestBackupRetentionRejectsUnboundReceiptsBeforeForgettingSnapshots(t *testing.T) {
	for _, damage := range []string{"missing", "foreign operation", "foreign target", "foreign run", "incomplete snapshot", "short snapshot"} {
		t.Run(damage, func(t *testing.T) {
			b, _ := backupIdentityFixture(t, false)
			ctx := context.Background()
			x := &backingUp{b: b.Backup, owner: "hello", target: "hello", bucket: "target-hello", staging: filepath.Join(b.StateDir, "backups", "hello"),
				receiptDir: filepath.Join(b.StateDir, "backups", ".receipts"), run: &backupRun{store: b.Store, app: "hello", kind: state.BackupRunBackup}, detail: func() string { return "test capture" }}
			if err := x.beginBackup(ctx); err != nil {
				t.Fatal(err)
			}
			x.receipt.Started = true
			x.receipt.Summary = &restic.Summary{SnapshotID: strings.Repeat("a", 64), BackupStart: time.Now().UTC(), TotalFiles: 1, TotalBytes: 10}
			switch damage {
			case "foreign operation":
				x.receipt.Operation = "another-operation"
			case "foreign target":
				x.receipt.Target = "another-app"
			case "foreign run":
				id, err := b.Store.StartBackupRun(ctx, "another-app", state.BackupRunBackup, time.Now().UTC())
				if err != nil {
					t.Fatal(err)
				}
				x.receipt.RunID = id
			case "incomplete snapshot":
				x.receipt.Summary = nil
			case "short snapshot":
				x.receipt.Summary.SnapshotID = "aaaaaaaa"
			}
			if err := writeRecoveryJSON(x.receiptPath(ctx), x.receipt); err != nil {
				t.Fatal(err)
			}
			if damage == "missing" {
				if err := os.Remove(x.receiptPath(ctx)); err != nil {
					t.Fatal(err)
				}
			}
			// Rebuild a plan after daemon death, without any in-memory run ID.
			x.run = &backupRun{store: b.Store, app: "hello", kind: state.BackupRunBackup}
			x.receipt, x.summary = nil, nil
			fake := dockertest.New(t)
			forgetCalls := 0
			fake.OnCreate = func(c dockertest.Container) {
				if c.Cmd[0] == "forget" {
					forgetCalls++
				}
				fake.Update(c.Name, func(c *dockertest.Container) { c.ExitAfter, c.Logs = 1, `[]` })
			}
			if err := x.retentionStep().Apply(ctx, io.Discard); err == nil {
				t.Fatal("retention accepted a missing or foreign snapshot binding")
			}
			if forgetCalls != 0 {
				t.Fatalf("retention removed snapshots before validating the receipt: %d", forgetCalls)
			}
		})
	}
}

func TestBackupSnapshotReplayRestartsWritersWithoutTakingAnotherSnapshot(t *testing.T) {
	b, _ := backupIdentityFixture(t, false)
	ctx := context.Background()
	rev, err := b.Store.RevisionWithStatus(ctx, "hello", state.RevisionActive)
	if err != nil || rev == nil {
		t.Fatalf("active revision: %+v %v", rev, err)
	}
	var m manifest.Manifest
	if err := json.Unmarshal(rev.Manifest, &m); err != nil {
		t.Fatal(err)
	}
	x := &backingUp{b: b.Backup, owner: "hello", target: "hello", bucket: "target-hello", staging: filepath.Join(b.StateDir, "backups", "hello"),
		receiptDir: filepath.Join(b.StateDir, "backups", ".receipts"), run: &backupRun{store: b.Store, app: "hello", kind: state.BackupRunBackup}}
	if err := x.beginBackup(ctx); err != nil {
		t.Fatal(err)
	}
	x.receipt.Started = true
	x.receipt.Summary = &restic.Summary{SnapshotID: strings.Repeat("a", 64), BackupStart: time.Now().UTC()}
	if err := writeRecoveryJSON(x.receiptPath(ctx), x.receipt); err != nil {
		t.Fatal(err)
	}
	fake := dockertest.New(t)
	fake.Add(dockertest.Container{ID: "paused-writer", Name: "bedrock-hello-web-active-code", Labels: map[string]string{docker.LabelOwner: docker.OwnerValue, docker.LabelApp: "hello", docker.LabelWorkload: "web"}})
	file := quiesceFile(b.Store.Path(), "hello")
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := savePausedWriters(file, []pausedWriter{{ID: "paused-writer", Name: "bedrock-hello-web-active-code"}}); err != nil {
		t.Fatal(err)
	}
	fake.OnCreate = func(c dockertest.Container) { t.Errorf("snapshot replay created a new container: %v", c.Cmd) }
	x.run, x.receipt, x.summary = &backupRun{store: b.Store, app: "hello", kind: state.BackupRunBackup}, nil, nil
	if err := x.appSnapshotStep(rev, &m).Apply(ctx, io.Discard); err != nil {
		t.Fatal(err)
	}
	writer, _ := fake.Container("bedrock-hello-web-active-code")
	if !writer.Running {
		t.Fatal("snapshot replay left the captured app's writer paused")
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatalf("snapshot replay retained its completed pause: %v", err)
	}
}

func TestBackupSnapshotReplayRefusesAnUncertainCapture(t *testing.T) {
	b, _ := backupIdentityFixture(t, true)
	ctx := context.Background()
	x := &backingUp{b: b.Backup, owner: manifest.ReservedApp, target: MachineTarget("old-host"), bucket: "target-machine-old-host",
		staging: filepath.Join(b.StateDir, "backups", "machine"), receiptDir: filepath.Join(b.StateDir, "backups", ".receipts"),
		run: &backupRun{store: b.Store, app: manifest.ReservedApp, kind: state.BackupRunBackup}}
	if err := x.beginBackup(ctx); err != nil {
		t.Fatal(err)
	}
	x.receipt.Started = true
	if err := writeRecoveryJSON(x.receiptPath(ctx), x.receipt); err != nil {
		t.Fatal(err)
	}
	fake := dockertest.New(t)
	backupCalls := 0
	fake.OnCreate = func(c dockertest.Container) {
		if c.Cmd[0] == "backup" {
			backupCalls++
		}
		fake.Update(c.Name, func(c *dockertest.Container) { c.ExitAfter = 1 })
	}
	x.run, x.receipt = &backupRun{store: b.Store, app: manifest.ReservedApp, kind: state.BackupRunBackup}, nil
	if err := x.machineSnapshotStep().Apply(ctx, io.Discard); err == nil || !strings.Contains(err.Error(), "interrupted without an exact snapshot receipt") {
		t.Fatalf("uncertain capture was retried: %v", err)
	}
	if backupCalls != 0 {
		t.Fatal("uncertain capture took a replacement snapshot")
	}
}

func TestBackupSnapshotRequiresItsPreparedReceiptBeforeTouchingNewerStaging(t *testing.T) {
	for _, machine := range []bool{false, true} {
		t.Run(map[bool]string{false: "app", true: "machine"}[machine], func(t *testing.T) {
			b, raw := backupIdentityFixture(t, machine)
			plan, err := b.Plan(context.Background(), raw)
			if err != nil {
				t.Fatal(err)
			}
			app := "hello"
			if machine {
				app = "machine"
			}
			staging := filepath.Join(b.StateDir, "backups", app)
			if err := os.MkdirAll(staging, 0o700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(staging, "newer.txt")
			if err := os.WriteFile(path, []byte("a newer prepared backup"), 0o600); err != nil {
				t.Fatal(err)
			}
			owner := backupSnapshotReceipt{Operation: "newer-backup", RunID: 321, Target: app}
			if err := writeRecoveryJSON(staging+".operation.json", owner); err != nil {
				t.Fatal(err)
			}
			fake := dockertest.New(t)
			calls := 0
			fake.OnCreate = func(c dockertest.Container) {
				calls++
				fake.Update(c.Name, func(c *dockertest.Container) { c.ExitAfter = 1 })
			}
			// An old journal can have prepare succeeded and snapshot pending,
			// without this version's prepared operation receipt.
			if err := plan.Steps[1].Apply(context.Background(), io.Discard); err == nil || !strings.Contains(err.Error(), "no prepared capture receipt") {
				t.Errorf("pending snapshot claimed staging without its prepared receipt: %v", err)
			}
			if calls != 0 {
				t.Errorf("unprepared snapshot reached Docker %d time(s)", calls)
			}
			var after backupSnapshotReceipt
			data, err := os.ReadFile(staging + ".operation.json")
			if err != nil || json.Unmarshal(data, &after) != nil || after.Operation != owner.Operation || after.RunID != owner.RunID {
				t.Fatalf("pending old snapshot replaced the newer staging owner: %+v %v", after, err)
			}
			if data, err := os.ReadFile(path); err != nil || string(data) != "a newer prepared backup" {
				t.Fatalf("pending old snapshot changed newer staging: %q %v", data, err)
			}
		})
	}
}

func TestBackupReplayPreservesStagingPreparedByANewerOperation(t *testing.T) {
	for _, phase := range []string{"pending snapshot", "pending retention", "completed retention"} {
		t.Run(phase, func(t *testing.T) {
			b, _ := backupIdentityFixture(t, false)
			ctx := context.Background()
			rev, err := b.Store.RevisionWithStatus(ctx, "hello", state.RevisionActive)
			if err != nil || rev == nil {
				t.Fatalf("active revision: %+v %v", rev, err)
			}
			var m manifest.Manifest
			if err := json.Unmarshal(rev.Manifest, &m); err != nil {
				t.Fatal(err)
			}
			x := &backingUp{b: b.Backup, owner: "hello", target: "hello", bucket: "target-hello", staging: filepath.Join(b.StateDir, "backups", "hello"),
				receiptDir: filepath.Join(b.StateDir, "backups", ".receipts"), run: &backupRun{store: b.Store, app: "hello", kind: state.BackupRunBackup}, detail: func() string { return "original backup" }}
			if err := x.appPrepareStep(rev, &m).Apply(ctx, io.Discard); err != nil {
				t.Fatal(err)
			}
			if phase != "pending snapshot" {
				x.receipt.Started = true
				x.receipt.Summary = &restic.Summary{SnapshotID: strings.Repeat("a", 64), BackupStart: time.Now().UTC(), TotalFiles: 7, TotalBytes: 1234}
				if err := writeRecoveryJSON(x.receiptPath(ctx), x.receipt); err != nil {
					t.Fatal(err)
				}
				if phase == "completed retention" {
					s := x.receipt.Summary
					if err := x.run.done(ctx, state.BackupRun{Snapshot: s.SnapshotID, SnapshotAt: s.BackupStart, Files: s.TotalFiles, Bytes: s.TotalBytes}); err != nil {
						t.Fatal(err)
					}
				}
			}
			newOwner := *x.receipt
			newOwner.Operation, newOwner.RunID = "newer-backup", 321
			if err := writeRecoveryJSON(x.staging+".operation.json", newOwner); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(x.staging, "newer.txt")
			if err := os.WriteFile(path, []byte("newer staged bytes"), 0o600); err != nil {
				t.Fatal(err)
			}
			x.run, x.receipt, x.summary = &backupRun{store: b.Store, app: "hello", kind: state.BackupRunBackup}, nil, nil
			fake := dockertest.New(t)
			calls := 0
			fake.OnCreate = func(c dockertest.Container) {
				calls++
				if phase != "pending retention" || c.Cmd[0] != "forget" {
					t.Errorf("backup replay reached an unexpected repository command: %v", c.Cmd)
				}
				fake.Update(c.Name, func(c *dockertest.Container) { c.ExitAfter, c.Logs = 1, `[]` })
			}
			if phase == "pending snapshot" {
				if err := x.appSnapshotStep(rev, &m).Apply(ctx, io.Discard); err == nil || !strings.Contains(err.Error(), "staging belongs to another operation") {
					t.Fatalf("snapshot replay accepted newer staging: %v", err)
				}
			} else if err := x.retentionStep().Apply(ctx, io.Discard); err != nil {
				t.Fatal("exact retention receipt failed when newer staging existed:", err)
			}
			if phase != "pending retention" && calls != 0 {
				t.Fatalf("backup replay reached Docker %d time(s)", calls)
			}
			if data, err := os.ReadFile(path); err != nil || string(data) != "newer staged bytes" {
				t.Fatalf("older backup deleted or changed newer staging: %q %v", data, err)
			}
			var after backupSnapshotReceipt
			data, err := os.ReadFile(x.staging + ".operation.json")
			if err != nil || json.Unmarshal(data, &after) != nil || after.Operation != newOwner.Operation || after.RunID != newOwner.RunID {
				t.Fatalf("older backup changed the newer staging owner: %+v %v", after, err)
			}
		})
	}
}
