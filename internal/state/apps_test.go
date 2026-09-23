package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
)

func TestActivateRotatesRevisions(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	t0 := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	for i, id := range []string{"r1", "r2", "r3"} {
		if err := s.SaveRevision(ctx, Revision{App: "site", ID: id, Status: RevisionFailed, Manifest: json.RawMessage(`{"app":"site"}`), Images: map[string]string{"web": "img:" + id}, Containers: map[string]string{"web": "bedrock-site-web-" + id}, CreatedAt: t0.Add(time.Duration(i) * time.Minute)}); err != nil {
			t.Fatal(err)
		}
		if err := s.Activate(ctx, "site", id, t0.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	revs, err := s.Revisions(ctx, "site")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]RevisionStatus{}
	for _, r := range revs {
		got[r.ID] = r.Status
	}
	if got["r3"] != RevisionActive || got["r2"] != RevisionPrevious || got["r1"] != RevisionRetired {
		t.Fatalf("statuses: %v", got)
	}
	if revs[0].ID != "r3" || revs[0].Images["web"] != "img:r3" || revs[0].Containers["web"] != "bedrock-site-web-r3" {
		t.Fatalf("newest first with its data: %+v", revs[0])
	}
	apps, err := s.Apps(ctx)
	if err != nil || len(apps) != 1 || apps[0].ActiveRevision != "r3" {
		t.Fatalf("apps: %v %+v", err, apps)
	}
	prev, err := s.RevisionWithStatus(ctx, "site", RevisionPrevious)
	if err != nil || prev == nil || prev.ID != "r2" {
		t.Fatalf("previous: %v %+v", err, prev)
	}
	active, err := s.ActiveRevisions(ctx)
	if err != nil || len(active) != 1 || active[0].ID != "r3" {
		t.Fatalf("active: %v %+v", err, active)
	}
	// Rolling back to r2 makes r3 the previous one.
	if err := s.Activate(ctx, "site", "r2", t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	r3, _ := s.GetRevision(ctx, "site", "r3")
	r2, _ := s.GetRevision(ctx, "site", "r2")
	if r3.Status != RevisionPrevious || r2.Status != RevisionActive {
		t.Fatalf("after rollback: r3=%s r2=%s", r3.Status, r2.Status)
	}
	if err := s.RemoveApp(ctx, "site"); err != nil {
		t.Fatal(err)
	}
	if apps, _ := s.Apps(ctx); len(apps) != 0 {
		t.Fatal("app not removed")
	}
}

func TestOpeningAnOlderStoreAddsNewColumns(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	// A store from before secrets_version existed.
	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.ExecContext(ctx, `CREATE TABLE revisions (app TEXT NOT NULL, id TEXT NOT NULL, status TEXT NOT NULL, manifest TEXT NOT NULL, images TEXT NOT NULL, containers TEXT NOT NULL, source TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL, PRIMARY KEY (app, id));
		INSERT INTO revisions (app, id, status, manifest, images, containers, created_at) VALUES ('site', 'r1', 'active', '{}', '{}', '{}', 1)`); err != nil {
		t.Fatal(err)
	}
	old.Close()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	rev, err := s.GetRevision(ctx, "site", "r1")
	if err != nil || rev.SecretsVersion != 0 {
		t.Fatalf("old row: %v %+v", err, rev)
	}
	rev.SecretsVersion = 3
	if err := s.SaveRevision(ctx, *rev); err != nil {
		t.Fatal(err)
	}
	if again, _ := s.GetRevision(ctx, "site", "r1"); again.SecretsVersion != 3 {
		t.Fatalf("column not usable: %+v", again)
	}
}

func TestRemovingAnAppForgetsItsRunsSignalsIncidentAndHookButNotItsBackups(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	for _, app := range []string{"hello", "other"} {
		if err := s.SaveRevision(ctx, Revision{App: app, ID: "r1", Status: RevisionActive, Manifest: json.RawMessage(`{}`), CreatedAt: now}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.StartJobRun(ctx, JobRun{App: app, Workload: "w", Revision: "r1", Kind: "cron", StartedAt: now}); err != nil {
			t.Fatal(err)
		}
		if err := s.AddSignal(ctx, Signal{Hour: now, App: app, Key: "w", Metric: SignalCPUPercent, Value: 1, Samples: 1}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.OpenIncident(ctx, Incident{Key: "app:" + app, Subject: app, Severity: SeverityCritical, Message: "down", OpenedAt: now}); err != nil {
			t.Fatal(err)
		}
		if err := s.SaveGitHook(ctx, GitHook{App: app, Repo: "r", Branch: "main", CreatedAt: now}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.StartBackupRun(ctx, app, BackupRunBackup, now); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.RemoveApp(ctx, "hello"); err != nil {
		t.Fatal(err)
	}
	if runs, _ := s.JobRuns(ctx, "hello", "", 10); len(runs) != 0 {
		t.Fatalf("job runs survived: %+v", runs)
	}
	if sigs, _ := s.Signals(ctx, "hello", now.Add(-time.Hour)); len(sigs) != 0 {
		t.Fatalf("signals survived: %+v", sigs)
	}
	if inc, _ := s.OpenIncidentByKey(ctx, "app:hello"); inc != nil {
		t.Fatalf("the incident survived: %+v", inc)
	}
	if h, _ := s.GitHookFor(ctx, "hello"); h != nil {
		t.Fatalf("the webhook survived: %+v", h)
	}
	if runs, _ := s.BackupRuns(ctx, "hello", "", 10); len(runs) != 1 {
		t.Fatalf("the backup history should stay: %+v", runs)
	}
	if runs, _ := s.JobRuns(ctx, "other", "", 10); len(runs) != 1 {
		t.Fatalf("another app's runs went: %+v", runs)
	}
	if inc, _ := s.OpenIncidentByKey(ctx, "app:other"); inc == nil {
		t.Fatal("another app's incident went")
	}
	if h, _ := s.GitHookFor(ctx, "other"); h == nil {
		t.Fatal("another app's webhook went")
	}
}
