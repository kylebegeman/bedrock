package cli

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apps "github.com/kylebegeman/bedrock/internal/app"
	"github.com/kylebegeman/bedrock/internal/state"
)

const helloWithDataJSON = `{"app":"hello","workloads":{"web":{"kind":"web","image":"x","port":80,"routes":[{"host":"hello.example.com"}]}},"data":{"postgres":{"version":"16"}}}`

// movable seeds a machine with hello deployed and storage set up, as a
// source of a move is.
func movable(t *testing.T) (string, *state.Store) {
	t.Helper()
	stateDir := t.TempDir()
	if _, errOut, code := withStdin(t, "kind=s3\nendpoint=http://127.0.0.1:9000\nkey_id=lane\nkey=hush\nbucket_prefix=lane\n", stateDir, "integration", "set", "storage"); code != 0 {
		t.Fatalf("storage: %d %q", code, errOut)
	}
	store, err := state.Open(filepath.Join(stateDir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	rev := state.Revision{App: "hello", ID: "r1", Status: state.RevisionActive, Manifest: json.RawMessage(helloWithDataJSON), CreatedAt: time.Now().UTC()}
	if err := store.SaveRevision(context.Background(), rev); err != nil {
		t.Fatal(err)
	}
	return stateDir, store
}

func TestMoveOutPlansTheBackupAndWritesNoHandoff(t *testing.T) {
	stateDir, store := movable(t)
	out, errOut, code := run(t, stateDir, "move", "out", "hello", "--plan")
	if code != 0 {
		t.Fatalf("code %d, out %q, err %q", code, out, errOut)
	}
	if !strings.Contains(out, "app.backup on hello") || !strings.Contains(out, "plan digest") || strings.Contains(out, "{") {
		t.Fatalf("stdout should hold the backup's plan and nothing else:\n%s", out)
	}
	if !strings.Contains(errOut, "then a handoff") {
		t.Fatalf("stderr should say what follows: %q", errOut)
	}
	if runs, _ := store.BackupRuns(context.Background(), "hello", "", 10); len(runs) != 0 {
		t.Fatalf("a plan ran a backup: %+v", runs)
	}
}

func TestMoveOutWithoutYesRefusesToBackUp(t *testing.T) {
	stateDir, store := movable(t)
	out, errOut, code := run(t, stateDir, "move", "out", "hello")
	if code != 1 || !strings.Contains(errOut, "add --yes") {
		t.Fatalf("code %d, out %q, err %q", code, out, errOut)
	}
	if out != "" {
		t.Fatalf("stdout should stay empty for the handoff: %q", out)
	}
	if runs, _ := store.BackupRuns(context.Background(), "hello", "", 10); len(runs) != 0 {
		t.Fatalf("a refused move ran a backup: %+v", runs)
	}
}

func TestMoveOutKeepsStdoutForTheHandoff(t *testing.T) {
	stateDir, store := movable(t)
	ctx := context.Background()
	t0 := time.Date(2026, 9, 22, 3, 0, 0, 0, time.UTC)
	id, err := store.StartBackupRun(ctx, "hello", state.BackupRunBackup, t0)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishBackupRun(ctx, id, state.BackupRun{OK: true, Snapshot: "abc12345", SnapshotAt: t0, FinishedAt: t0.Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	out, errOut, code := run(t, stateDir, "move", "out", "hello", "--no-backup")
	if code != 0 {
		t.Fatalf("code %d, out %q, err %q", code, out, errOut)
	}
	h, err := apps.ReadHandoff(strings.NewReader(out))
	if err != nil {
		t.Fatalf("stdout isn't a handoff: %v\n%s", err, out)
	}
	if h.App != "hello" || h.Snapshot != "abc12345" || h.Bucket != "lane-hello" {
		t.Fatalf("handoff: %+v", h)
	}
	if !strings.Contains(errOut, "snapshot abc12345") {
		t.Fatalf("the description belongs on stderr: %q", errOut)
	}
}
