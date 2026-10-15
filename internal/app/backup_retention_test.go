package app

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kylebegeman/helios/internal/docker/dockertest"
	"github.com/kylebegeman/helios/internal/kernel"
	"github.com/kylebegeman/helios/internal/restic"
	"github.com/kylebegeman/helios/internal/state"
)

// retentionRepository answers restic as an existing repository would, with
// forget behaving as the test says: "hang" never exits, "fail" exits with
// restic's error, anything else succeeds.
func retentionRepository(t *testing.T, summary restic.Summary, forget *string) (*dockertest.Server, *[]string) {
	t.Helper()
	fake := dockertest.New(t)
	var commands []string
	fake.OnCreate = func(c dockertest.Container) {
		commands = append(commands, c.Cmd[0])
		fake.Update(c.Name, func(c *dockertest.Container) {
			c.ExitAfter = 1
			switch c.Cmd[0] {
			case "init":
				c.ExitCode, c.Logs = 1, `{"message_type":"exit_error","code":1,"message":"Fatal: config file already exists"}`
			case "backup":
				body, _ := json.Marshal(struct {
					Type string `json:"message_type"`
					restic.Summary
				}{"summary", summary})
				c.Logs = string(body)
			case "forget":
				switch *forget {
				case "hang":
					c.ExitAfter, c.Logs = 0, `{"message_type":"status"}`
				case "fail":
					c.ExitCode, c.Logs = 1, `{"message_type":"exit_error","code":1,"message":"Fatal: unable to create lock in backend: repository is already locked exclusively"}`
				default:
					c.Logs = `[{"remove":[{"id":"old"}]}]`
				}
			}
		})
	}
	return fake, &commands
}

func TestBackupDefersRetentionThatFailsOrRunsOutOfTime(t *testing.T) {
	for _, machine := range []bool{false, true} {
		for _, forget := range []string{"hang", "fail"} {
			t.Run(map[bool]string{false: "app", true: "machine"}[machine]+" "+forget, func(t *testing.T) {
				b, raw := backupIdentityFixture(t, machine)
				b.Deadlines = restic.Deadlines{Forget: 50 * time.Millisecond}
				at := time.Now().UTC().Truncate(time.Second)
				summary := restic.Summary{SnapshotID: strings.Repeat("c", 64), BackupStart: at, BackupEnd: at.Add(time.Second), TotalFiles: 3, TotalBytes: 300}
				behavior := forget
				fake, commands := retentionRepository(t, summary, &behavior)
				reg := kernel.Registry{}
				reg.Add(b)
				engine := kernel.New(b.Store, reg, "daemon")
				var lines []string
				started := time.Now()
				receipt, err := engine.Run(context.Background(), BackupKind, raw, func(ev kernel.Event) {
					if ev.Line != "" {
						lines = append(lines, ev.Line)
					}
				})
				if took := time.Since(started); took > 10*time.Second {
					t.Fatalf("retention held the operation for %s", took)
				}
				if err != nil || receipt.Status != state.Succeeded {
					t.Fatalf("a kept snapshot must not fail the backup: %+v %v", receipt, err)
				}
				if want := []string{"init", "unlock", "backup", "forget"}; !slices.Equal(*commands, want) {
					t.Fatalf("stale locks must be cleared before the capture: %v, want %v", *commands, want)
				}
				runs, err := b.Store.BackupRuns(context.Background(), "", state.BackupRunBackup, 10)
				if err != nil || len(runs) != 1 {
					t.Fatalf("runs: %+v %v", runs, err)
				}
				run := runs[0]
				wantReason := map[string]string{"hang": "restic forget did not finish within 50ms and was stopped", "fail": "repository is already locked exclusively"}[forget]
				if !run.OK || !run.Finished() || run.Error != "" || run.Snapshot != summary.SnapshotID || !strings.Contains(run.Detail, retentionDeferred+": ") || !strings.Contains(run.Detail, wantReason) {
					t.Fatalf("the run must keep its snapshot and say retention was deferred: %+v", run)
				}
				if output := strings.Join(lines, "\n"); !strings.Contains(output, retentionDeferred) || !strings.Contains(output, "next backup applies retention again") {
					t.Fatalf("the operation output must explain the deferral: %q", output)
				}
				for _, call := range fake.Calls() {
					if strings.Contains(call, "--remove-all") {
						t.Fatalf("live locks must never be forced: %s", call)
					}
				}
				created, removed := 0, 0
				for _, call := range fake.Calls() {
					switch {
					case strings.HasPrefix(call, "POST /containers/create"):
						created++
					case strings.HasPrefix(call, "DELETE /containers/"):
						removed++
					}
				}
				if created == 0 || removed < created {
					t.Fatalf("a restic helper outlived the backup: %d created, %d removed", created, removed)
				}

				// The next backup applies retention again.
				behavior = "succeed"
				*commands = nil
				second, err := kernel.New(b.Store, reg, "daemon").Run(context.Background(), BackupKind, raw, func(kernel.Event) {})
				if err != nil || second.Status != state.Succeeded {
					t.Fatalf("next backup: %+v %v", second, err)
				}
				if !slices.Contains(*commands, "forget") {
					t.Fatalf("the next backup did not retry retention: %v", *commands)
				}
				last, err := b.Store.LastBackupRun(context.Background(), runs[0].App, state.BackupRunBackup)
				if err != nil || last == nil || !last.OK || strings.Contains(last.Detail, retentionDeferred) {
					t.Fatalf("a completed retention must not carry the old deferral: %+v %v", last, err)
				}
			})
		}
	}
}

func TestBackupRetentionInterruptedByTheOperationIsNotDeferred(t *testing.T) {
	b, raw := backupIdentityFixture(t, true)
	at := time.Now().UTC().Truncate(time.Second)
	summary := restic.Summary{SnapshotID: strings.Repeat("d", 64), BackupStart: at, TotalFiles: 1, TotalBytes: 1}
	behavior := "hang"
	fake, _ := retentionRepository(t, summary, &behavior)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	previous := fake.OnCreate
	fake.OnCreate = func(c dockertest.Container) {
		previous(c)
		// A daemon shutdown during retention is an interruption to resume,
		// not a deferral to record.
		if c.Cmd[0] == "forget" {
			cancel()
		}
	}
	reg := kernel.Registry{}
	reg.Add(b)
	receipt, err := kernel.New(b.Store, reg, "daemon").Run(ctx, BackupKind, raw, func(kernel.Event) {})
	if err == nil && receipt != nil && receipt.Status == state.Succeeded {
		t.Fatal("an interrupted retention must not complete the operation")
	}
	last, err := b.Store.LastBackupRun(context.Background(), "bedrock", state.BackupRunBackup)
	if err != nil || last == nil || last.OK || strings.Contains(last.Detail, retentionDeferred) {
		t.Fatalf("an interruption was recorded as a deferral: %+v %v", last, err)
	}
}
