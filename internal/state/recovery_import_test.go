package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMachineRecoveryImportsInactiveAppAndHistoryWithoutReplacingAuthority(t *testing.T) {
	ctx := context.Background()
	source := open(t)
	target := open(t)
	now := time.Now().UTC()
	original := Revision{App: "site", ID: "old", Status: RevisionActive, Manifest: json.RawMessage(`{"app":"site"}`), Containers: map[string]string{"web": "source-container"}, Images: map[string]string{"web": "image"}, SecretsVersion: 3, CreatedAt: now}
	source.SaveRevision(ctx, original)
	source.Activate(ctx, "site", "old", now)
	source.SaveGitHook(ctx, GitHook{App: "site", Repo: "repo", Branch: "main", CreatedAt: now})
	source.AddWatch(ctx, "https://site.example.com", now)
	source.StartBackupRun(ctx, "site", BackupRunBackup, now)
	source.StartJobRun(ctx, JobRun{App: "site", Workload: "cron", Revision: "old", Kind: "cron", Container: "old-job", StartedAt: now})
	target.CreateOperation(ctx, NewOperation{ID: "recovery-operation", Kind: "machine.recover", Target: "machine", Input: json.RawMessage(`{}`), Plan: json.RawMessage(`{}`), StepNames: []string{"import"}, CreatedAt: now})
	target.RecordIntegrationUse(ctx, "target-integration", "keep", now)
	candidate := Revision{App: "site", ID: "recovered", Status: RevisionActive, Manifest: original.Manifest, Containers: map[string]string{"web": "private-target-container"}, Images: original.Images, SecretsVersion: 3, CreatedAt: now}
	mapping := map[string]map[int]int{"site": {3: 3}}
	for i := 0; i < 2; i++ {
		if err := target.ImportMachineRecovery(ctx, source.Path(), "snapshot-one", mapping, []Revision{candidate}); err != nil {
			t.Fatal(err)
		}
	}
	apps, _ := target.Apps(ctx)
	if len(apps) != 1 || apps[0].ActiveRevision != "" {
		t.Fatalf("recovered app routed too soon: %+v", apps)
	}
	recovered, _ := target.GetRevision(ctx, "site", "recovered")
	old, _ := target.GetRevision(ctx, "site", "old")
	if recovered.Status != RevisionFailed || recovered.Containers["web"] != "private-target-container" || old.Status != RevisionRetired || len(old.Containers) != 0 || old.SecretsVersion != 3 {
		t.Fatalf("recovered %+v old %+v", recovered, old)
	}
	sourceOld, _ := source.GetRevision(ctx, "site", "old")
	if sourceOld.Status != RevisionActive || sourceOld.Containers["web"] != "source-container" {
		t.Fatal("source history mutated")
	}
	operations, _ := target.List(ctx, 10)
	if len(operations) != 1 || operations[0].ID != "recovery-operation" {
		t.Fatalf("target journal replaced: %+v", operations)
	}
	integrations, _ := target.IntegrationUses(ctx)
	if integrations["target-integration"].Purpose != "keep" {
		t.Fatal("integration authority lost")
	}
	runs, _ := target.BackupRuns(ctx, "site", "", 10)
	if len(runs) != 1 || runs[0].FinishedAt.IsZero() {
		t.Fatalf("backup history duplicated or left running: %+v", runs)
	}
	jobs, _ := target.JobRuns(ctx, "site", "", 10)
	if len(jobs) != 1 || jobs[0].Container != "" || jobs[0].FinishedAt.IsZero() {
		t.Fatalf("source job resurrected: %+v", jobs)
	}
}
func TestMachineRecoveryConflictsAndMissingSecretsRollBackEverything(t *testing.T) {
	ctx := context.Background()
	source := open(t)
	now := time.Now().UTC()
	source.SaveRevision(ctx, Revision{App: "site", ID: "old", Status: RevisionActive, SecretsVersion: 2, CreatedAt: now})
	source.Activate(ctx, "site", "old", now)
	target := open(t)
	if err := target.ImportMachineRecovery(ctx, source.Path(), "snapshot", nil, nil); err == nil || !strings.Contains(err.Error(), "resealed secrets") {
		t.Fatalf("%v", err)
	}
	apps, _ := target.Apps(ctx)
	if len(apps) != 0 {
		t.Fatal("failed import wrote app")
	}
	target.SaveRevision(ctx, Revision{App: "site", ID: "target", Status: RevisionFailed, CreatedAt: now})
	if err := target.ImportMachineRecovery(ctx, source.Path(), "snapshot", map[string]map[int]int{"site": {2: 2}}, nil); err == nil || !strings.Contains(err.Error(), "existing app") {
		t.Fatalf("%v", err)
	}
	revisions, _ := target.Revisions(ctx, "site")
	if len(revisions) != 1 || revisions[0].ID != "target" {
		t.Fatalf("target overwritten: %+v", revisions)
	}
}

func TestHistoricalRecoveryMigratesOnlyAnIsolatedCopy(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "historical.db")
	db, _ := sql.Open("sqlite", path)
	_, err := db.Exec(`CREATE TABLE revisions(app TEXT NOT NULL,id TEXT NOT NULL,status TEXT NOT NULL,manifest TEXT NOT NULL,images TEXT NOT NULL,containers TEXT NOT NULL,source TEXT NOT NULL DEFAULT '',created_at INTEGER NOT NULL,PRIMARY KEY(app,id));INSERT INTO revisions VALUES('site','old','active','{}','{}','{}','source',123);`)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	before, _ := os.ReadFile(path)
	target := open(t)
	if err := target.ImportMachineRecovery(ctx, path, "historical", nil, nil); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("historical archive migrated in place")
	}
	revision, err := target.GetRevision(ctx, "site", "old")
	if err != nil || revision.Status != RevisionRetired || revision.SecretsVersion != 0 {
		t.Fatalf("%+v %v", revision, err)
	}
}
