package state

import (
	"context"
	"testing"
	"time"
)

func TestBackupRunsRememberTheLastGoodOne(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	t0 := time.Date(2026, 9, 19, 3, 0, 0, 0, time.UTC)
	id1, err := s.StartBackupRun(ctx, "dw", BackupRunBackup, t0)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.FinishBackupRun(ctx, id1, BackupRun{OK: true, Snapshot: "abcd1234", SnapshotAt: t0, Files: 3, Bytes: 4096, Detail: "database and 3 files", FinishedAt: t0.Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	id2, _ := s.StartBackupRun(ctx, "dw", BackupRunBackup, t0.Add(24*time.Hour))
	_ = s.FinishBackupRun(ctx, id2, BackupRun{Error: "bucket unreachable", FinishedAt: t0.Add(24*time.Hour + time.Second)})
	id3, _ := s.StartBackupRun(ctx, "dw", BackupRunDrill, t0.Add(25*time.Hour))
	_ = id3
	last, err := s.LastBackupRun(ctx, "dw", BackupRunBackup)
	if err != nil || last == nil || last.ID != id2 || last.OK || last.Error != "bucket unreachable" {
		t.Fatalf("last: %+v %v", last, err)
	}
	good, err := s.LastGoodBackupRun(ctx, "dw", BackupRunBackup)
	if err != nil || good == nil || good.ID != id1 || good.Snapshot != "abcd1234" || good.Bytes != 4096 || !good.SnapshotAt.Equal(t0) {
		t.Fatalf("good: %+v %v", good, err)
	}
	drill, _ := s.LastBackupRun(ctx, "dw", BackupRunDrill)
	if drill == nil || drill.Finished() {
		t.Fatalf("an unfinished drill: %+v", drill)
	}
	all, _ := s.BackupRuns(ctx, "", "", 10)
	if len(all) != 3 || all[0].Kind != BackupRunDrill {
		t.Fatalf("all runs newest first: %+v", all)
	}
	if none, _ := s.LastGoodBackupRun(ctx, "other", BackupRunBackup); none != nil {
		t.Fatal("found a run for an unknown app")
	}
	if err := s.ForgetBackupRuns(ctx, "dw"); err != nil {
		t.Fatal(err)
	}
	if all, _ := s.BackupRuns(ctx, "dw", "", 10); len(all) != 0 {
		t.Fatal("runs not forgotten")
	}
}

func TestIncidentsOpenUpdateAndResolve(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	t0 := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	id, err := s.OpenIncident(ctx, Incident{Key: "app:hello", Subject: "hello", Severity: SeverityCritical, Message: "web isn't running", OpenedAt: t0, Observations: 3})
	if err != nil {
		t.Fatal(err)
	}
	inc, err := s.OpenIncidentByKey(ctx, "app:hello")
	if err != nil || inc == nil || inc.ID != id || !inc.Open() || inc.Observations != 3 {
		t.Fatalf("%+v %v", inc, err)
	}
	if other, _ := s.OpenIncidentByKey(ctx, "app:other"); other != nil {
		t.Fatal("found an incident for another key")
	}
	inc.Message = "web isn't running; the check fails"
	inc.NotifiedAt = t0.Add(time.Minute)
	inc.Observations = 4
	if err := s.UpdateIncident(ctx, *inc); err != nil {
		t.Fatal(err)
	}
	open, _ := s.OpenIncidents(ctx)
	if len(open) != 1 || open[0].Message != inc.Message || !open[0].NotifiedAt.Equal(t0.Add(time.Minute)) {
		t.Fatalf("%+v", open)
	}
	inc.ResolvedAt = t0.Add(10 * time.Minute)
	_ = s.UpdateIncident(ctx, *inc)
	if open, _ := s.OpenIncidents(ctx); len(open) != 0 {
		t.Fatal("still open")
	}
	history, _ := s.Incidents(ctx, 5)
	if len(history) != 1 || history[0].Open() {
		t.Fatalf("%+v", history)
	}
}

func TestWatchesAndIntegrationUses(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	t0 := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	if err := s.AddWatch(ctx, "https://core.begam.in/healthz", t0); err != nil {
		t.Fatal(err)
	}
	_ = s.AddWatch(ctx, "https://core.begam.in/healthz", t0.Add(time.Hour)) // again is fine
	ws, _ := s.Watches(ctx)
	if len(ws) != 1 || !ws[0].AddedAt.Equal(t0) {
		t.Fatalf("%+v", ws)
	}
	if err := s.RemoveWatch(ctx, "https://nowhere/"); err != ErrNotFound {
		t.Fatalf("removing an unknown watch: %v", err)
	}
	if err := s.RemoveWatch(ctx, "https://core.begam.in/healthz"); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordIntegrationUse(ctx, "storage", "backup dw", t0); err != nil {
		t.Fatal(err)
	}
	_ = s.RecordIntegrationUse(ctx, "storage", "backup hello", t0.Add(time.Hour))
	uses, _ := s.IntegrationUses(ctx)
	if u := uses["storage"]; u.Purpose != "backup hello" || !u.UsedAt.Equal(t0.Add(time.Hour)) {
		t.Fatalf("%+v", uses)
	}
}

func TestSignalsRollUpAndPrune(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	hour := time.Date(2026, 9, 19, 16, 0, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		if err := s.AddSignal(ctx, Signal{Hour: hour.Add(time.Duration(i) * 10 * time.Minute), App: "hello", Key: "hello.lane.begam.in", Metric: SignalRequests, Value: 10, Samples: 1}); err != nil {
			t.Fatal(err)
		}
	}
	_ = s.MaxSignal(ctx, Signal{Hour: hour, App: "hello", Key: "web", Metric: SignalMemoryMax, Value: 100})
	_ = s.MaxSignal(ctx, Signal{Hour: hour, App: "hello", Key: "web", Metric: SignalMemoryMax, Value: 80})
	_ = s.AddSignal(ctx, Signal{Hour: hour.Add(-100 * 24 * time.Hour), App: "hello", Key: "x", Metric: SignalRequests, Value: 1, Samples: 1})
	rows, _ := s.Signals(ctx, "hello", hour.Add(-time.Hour))
	if len(rows) != 2 || rows[0].Value != 30 || rows[0].Samples != 3 || rows[1].Value != 100 {
		t.Fatalf("%+v", rows)
	}
	n, err := s.PruneSignals(ctx, hour.Add(-90*24*time.Hour))
	if err != nil || n != 1 {
		t.Fatalf("pruned %d %v", n, err)
	}
	if err := s.BackupTo(ctx, s.Path()+".copy"); err != nil {
		t.Fatal(err)
	}
	copy, err := Open(s.Path() + ".copy")
	if err != nil {
		t.Fatal(err)
	}
	defer copy.Close()
	if rows, _ := copy.Signals(ctx, "hello", hour.Add(-time.Hour)); len(rows) != 2 {
		t.Fatalf("the copy: %+v", rows)
	}
}
